package slotmgr

import (
	"context"
	"expvar"
	"fmt"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/samber/lo"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

var WatchInterval = 5 * time.Minute

var log = logging.Logger("slotmgr")

type slot struct {
	slotOffset uint64
	sectors    map[abi.SectorID]struct{}

	// work is true when the slot is actively used for batch sealing (P1/P2)
	// false when not is use AND when sectors in the slot are waiting for finalization
	// When work is set to false, slot.sectors should be a superset of batch refs in the DB
	// and a local process should periodically check the db for removed refs and remove them from the slot
	work bool

	// trimming is true while the slot's storage is being deallocated (NVMe
	// TRIM) after the slot became free. The slot is not handed out until the
	// trim finishes.
	trimming bool
}

// free reports whether the slot can be handed out for new work.
func (st *slot) free() bool {
	return len(st.sectors) == 0 && !st.trimming
}

type SlotMgr struct {
	db      *harmonydb.DB
	machine string

	slots []*slot

	lk   sync.Mutex
	cond *sync.Cond

	// trim, when set, deallocates a slot's storage. It is called whenever a
	// slot becomes free, outside the lock.
	trim func(slotOff uint64) error
}

// NewSlotMgr creates the SlotMgr and initializes local references from the DB.
func NewSlotMgr(db *harmonydb.DB, machineHostAndPort string, slotOffs []uint64) (*SlotMgr, error) {
	slots := make([]*slot, len(slotOffs))
	for i, off := range slotOffs {
		slots[i] = &slot{
			slotOffset: off,
			sectors:    make(map[abi.SectorID]struct{}),
		}
	}

	// load from DB
	var slotRefs []struct {
		SpID         int64  `db:"sp_id"`
		SectorNumber int64  `db:"sector_number"`
		PipelineSlot uint64 `db:"pipeline_slot"`
	}
	err := db.Select(context.Background(), &slotRefs,
		`SELECT sp_id, sector_number, pipeline_slot FROM batch_sector_refs WHERE machine_host_and_port = $1`,
		machineHostAndPort)
	if err != nil {
		return nil, xerrors.Errorf("getting slot refs: %w", err)
	}

	for _, ref := range slotRefs {
		st, found := lo.Find(slots, func(st *slot) bool {
			return st.slotOffset == ref.PipelineSlot
		})
		if !found {
			return nil, xerrors.Errorf("slot %d not found", ref.PipelineSlot)
		}
		st.sectors[abi.SectorID{
			Miner:  abi.ActorID(ref.SpID),
			Number: abi.SectorNumber(ref.SectorNumber),
		}] = struct{}{}
	}

	// Record the total # of slots as "available capacity" initially
	stats.Record(context.Background(), SlotMgrMeasures.SlotsAvailable.M(int64(len(slotOffs))))

	sm := &SlotMgr{
		db:      db,
		machine: machineHostAndPort,

		slots: slots,
	}
	sm.cond = sync.NewCond(&sm.lk)

	// Expose entire slotmgr as expvar
	expvar.Publish("slotmgr", expvar.Func(func() any {
		sm.lk.Lock()
		defer sm.lk.Unlock()

		type jsonSlot struct {
			SlotOffset uint64         `json:"slot_offset"`
			Work       bool           `json:"work"`
			Trimming   bool           `json:"trimming"`
			Sectors    []abi.SectorID `json:"sectors"`
		}
		type jsonSm struct {
			Slots []jsonSlot `json:"slots"`
		}

		slc := make([]jsonSlot, len(sm.slots))
		for i, slt := range sm.slots {
			slc[i] = jsonSlot{
				SlotOffset: slt.slotOffset,
				Work:       slt.work,
				Trimming:   slt.trimming,
				Sectors:    lo.Keys(slt.sectors),
			}
		}

		return jsonSm{Slots: slc}
	}))

	// Start a background watch loop
	go sm.watchSlots()

	return sm, nil
}

// EnableTrim makes the manager deallocate a slot's storage whenever the slot
// becomes free (all its sectors finalized, or its batch aborted). Slots which
// are free right now - no sectors waiting for C1/finalization - are trimmed
// immediately, which clears data left behind by previous runs.
//
// A slot is not handed out by Get while it is being trimmed.
func (s *SlotMgr) EnableTrim(trim func(slotOff uint64) error) {
	s.lk.Lock()
	defer s.lk.Unlock()

	s.trim = trim
	for _, slt := range s.slots {
		if len(slt.sectors) == 0 && !slt.work {
			s.startTrimLocked(slt, "startup")
		}
	}
}

// slotFreedLocked is called with the lock held when a slot transitions to
// having no sectors.
func (s *SlotMgr) slotFreedLocked(slt *slot, reason string) {
	if s.trim != nil {
		s.startTrimLocked(slt, reason)
		return
	}
	s.cond.Signal()
}

func (s *SlotMgr) startTrimLocked(slt *slot, reason string) {
	if slt.trimming {
		return
	}
	slt.trimming = true
	s.recordSlotMetrics(slt)

	trim := s.trim
	go func() {
		start := time.Now()
		err := trim(slt.slotOffset)
		if err != nil {
			log.Errorw("trimming slot", "slot", slt.slotOffset, "reason", reason, "took", time.Since(start), "error", err)
			stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		} else {
			log.Infow("trimmed slot", "slot", slt.slotOffset, "reason", reason, "took", time.Since(start))
		}

		s.lk.Lock()
		slt.trimming = false
		s.recordSlotMetrics(slt)
		s.cond.Broadcast()
		s.lk.Unlock()

		stats.Record(context.Background(), SlotMgrMeasures.SlotsAvailable.M(int64(s.Available())))
	}()
}

func (s *SlotMgr) watchSlots() {
	for {
		time.Sleep(WatchInterval)
		if err := s.watchSingle(); err != nil {
			log.Errorf("watchSingle failed: %s", err)
			// Track errors
			stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		}
	}
}

func (s *SlotMgr) watchSingle() error {
	s.lk.Lock()
	defer s.lk.Unlock()

	for _, slt := range s.slots {
		// only watch slots which are NOT worked on and have sectors
		if slt.work || len(slt.sectors) == 0 {
			continue
		}

		var refs []struct {
			SpID         int64 `db:"sp_id"`
			SectorNumber int64 `db:"sector_number"`
		}

		err := s.db.Select(context.Background(), &refs,
			`SELECT sp_id, sector_number FROM batch_sector_refs WHERE pipeline_slot = $1 AND machine_host_and_port = $2`,
			slt.slotOffset, s.machine)
		if err != nil {
			return xerrors.Errorf("getting refs: %w", err)
		}

		// find refs which are in the slot but not in the db
		for id := range slt.sectors {
			found := false
			for _, ref := range refs {
				if (abi.SectorID{Miner: abi.ActorID(ref.SpID), Number: abi.SectorNumber(ref.SectorNumber)}) == id {
					found = true
					break
				}
			}
			if !found {
				log.Warnf("slot %d: removing local sector ref %v", slt.slotOffset, id)
				delete(slt.sectors, id)
			}
		}

		// If we removed everything from the slot, release it
		if len(slt.sectors) == 0 {
			s.slotFreedLocked(slt, "refs removed")
		}

		// Always re-record metrics if we might have changed the slot
		s.recordSlotMetrics(slt)
	}

	return nil
}

// Get allocates a slot for work. Called from a batch sealing task.
func (s *SlotMgr) Get(ids []abi.SectorID) uint64 {
	s.lk.Lock()

	if len(ids) == 0 {
		s.lk.Unlock()
		panic("no ids")
	}

	log.Infow("acquiring slot", "sectors", ids)

	for {
		for _, slt := range s.slots {
			// pick the first free slot
			if slt.free() {
				// Fill it
				for _, id := range ids {
					slt.sectors[id] = struct{}{}
				}
				slt.work = true
				soff := slt.slotOffset

				// Record metrics (slots acquired, available count, per-slot)
				s.recordSlotMetrics(slt)

				s.lk.Unlock()
				stats.Record(context.Background(), SlotMgrMeasures.SlotsAcquired.M(1))
				stats.Record(context.Background(), SlotMgrMeasures.SlotsAvailable.M(int64(s.Available())))

				return soff
			}
		}
		s.cond.Wait() // wait until a slot is freed
	}
}

// MarkWorkDone marks a slot as no longer being actively used for batch sealing
// This is when sectors start waiting for finalization (After C1 outputs were produced)
func (s *SlotMgr) MarkWorkDone(slotOff uint64) error {
	s.lk.Lock()
	defer s.lk.Unlock()

	sl, found := lo.Find(s.slots, func(st *slot) bool {
		return st.slotOffset == slotOff
	})
	if !found {
		stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		return xerrors.Errorf("slot %d not found", slotOff)
	}

	sl.work = false
	s.recordSlotMetrics(sl)
	return nil
}

// AbortSlot marks a slot which was used for work as immediately free
func (s *SlotMgr) AbortSlot(slotOff uint64) error {
	s.lk.Lock()
	defer s.lk.Unlock()

	sl, found := lo.Find(s.slots, func(st *slot) bool {
		return st.slotOffset == slotOff
	})
	if !found {
		stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		return xerrors.Errorf("slot %d not found", slotOff)
	}

	// Freed up => increment "slots released"
	stats.Record(context.Background(), SlotMgrMeasures.SlotsReleased.M(1))

	sl.sectors = make(map[abi.SectorID]struct{})
	sl.work = false
	s.slotFreedLocked(sl, "aborted")

	s.recordSlotMetrics(sl)
	return nil
}

// SectorDone removes a sector from a slot. Called when a sector is fully finalized.
func (s *SlotMgr) SectorDone(ctx context.Context, slotOff uint64, id abi.SectorID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM batch_sector_refs WHERE sp_id = $1 AND sector_number = $2`,
		id.Miner, id.Number)
	if err != nil {
		stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		return xerrors.Errorf("deleting batch refs: %w", err)
	}

	s.lk.Lock()
	defer s.lk.Unlock()

	sl, found := lo.Find(s.slots, func(st *slot) bool {
		return st.slotOffset == slotOff
	})
	if !found {
		stats.Record(context.Background(), SlotMgrMeasures.SlotErrors.M(1))
		return xerrors.Errorf("slot %d not found", slotOff)
	}

	_, had := sl.sectors[id]
	delete(sl.sectors, id)

	// If this freed up the slot completely, release it. The background watcher
	// may already have removed the sector (and released the slot) after the DB
	// ref was deleted above.
	if had && len(sl.sectors) == 0 {
		s.slotFreedLocked(sl, "sectors finalized")
	}

	s.recordSlotMetrics(sl)
	return nil
}

// Available returns how many slots are currently free (i.e. 0 sectors).
func (s *SlotMgr) Available() int {
	s.lk.Lock()
	defer s.lk.Unlock()

	var available int
	for _, slt := range s.slots {
		if slt.free() {
			available++
		}
	}
	return available
}

// recordSlotMetrics updates per-slot metrics.
func (s *SlotMgr) recordSlotMetrics(slt *slot) {
	ctx, _ := tag.New(context.Background(), tag.Upsert(KeySlotOffset, fmt.Sprintf("%d", slt.slotOffset)))

	inUse := int64(0)
	if slt.work {
		inUse = 1
	}

	stats.Record(ctx,
		SlotMgrSlotMeasures.SlotInUse.M(inUse),
		SlotMgrSlotMeasures.SlotSectorCount.M(int64(len(slt.sectors))),
	)
}
