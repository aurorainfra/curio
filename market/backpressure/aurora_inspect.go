package backpressure

// Aurora: read-only inspection of market backpressure for diagnostics
// (aurora/nodediag). Reports every counter the upstream checks compare,
// with its limit, which conditions trip, the fresh verdict of the upstream
// check functions, and the cached values deals currently see. The counter
// queries mirror the upstream checks in backpressure.go; a disagreement
// with the upstream verdict is flagged so query drift after a rebase shows
// up instead of misleading.

import (
	"context"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// cache TTLs used by the upstream *Pressure functions
var auroraKeyTTL = map[string]time.Duration{
	sectorBackpressureKey: 10 * time.Minute,
	mk12BackpressureKey:   2 * time.Minute,
	mk20BackpressureKey:   2 * time.Minute,
}

var auroraKeyName = map[string]string{
	sectorBackpressureKey: "sector",
	mk12BackpressureKey:   "mk12",
	mk20BackpressureKey:   "mk20",
}

var auroraRegistry struct {
	mu    sync.Mutex
	insts []*auroraInstance
}

type auroraInstance struct {
	c     *CachedBackPressure
	mu    sync.Mutex
	setAt map[string]time.Time
}

// auroraRegister tracks a backpressure cache so inspection can report what
// deals currently see and since when.
func auroraRegister(c *CachedBackPressure) {
	inst := &auroraInstance{c: c, setAt: map[string]time.Time{}}
	c.cache.SetNewItemCallback(func(key string, _ interface{}) {
		inst.mu.Lock()
		inst.setAt[key] = time.Now()
		inst.mu.Unlock()
	})
	auroraRegistry.mu.Lock()
	auroraRegistry.insts = append(auroraRegistry.insts, inst)
	auroraRegistry.mu.Unlock()
}

type Condition struct {
	Name  string
	Value int64
	// Limit is the config value compared against; the condition trips
	// when Value > Limit.
	Limit    int64
	LimitCfg string
	// Disabled: a zero limit turns this condition off.
	Disabled bool `json:",omitempty"`
	Tripped  bool
	Note     string `json:",omitempty"`
}

type Check struct {
	// Name: sector (sealing pipeline, applies to MK12 and MK20), mk12 or
	// mk20 (deal pipelines).
	Name       string
	Conditions []Condition
	Tripped    bool
	// Upstream is the fresh verdict of the upstream check function.
	Upstream      *bool  `json:",omitempty"`
	UpstreamError string `json:",omitempty"`
	Note          string `json:",omitempty"`
}

type CachedValue struct {
	Check string
	Value bool
	SetAt *time.Time `json:",omitempty"`
	// ExpiresIn is when the cached value is recomputed; until then deals
	// see Value even if the counters changed.
	ExpiresIn string `json:",omitempty"`
}

type Report struct {
	Snap   bool
	Checks []Check
	// Cached lists the cached verdicts of each backpressure cache on this
	// node (one per market instance).
	Cached [][]CachedValue `json:",omitempty"`
	// MK12Busy / MK20Busy are the fresh verdicts of the deal paths:
	// mk12 || sector and mk20 || sector.
	MK12Busy bool
	MK20Busy bool
	Notes    []string `json:",omitempty"`
}

func cond(name, cfgName string, value, limit int64, zeroDisables bool) Condition {
	c := Condition{Name: name, Value: value, Limit: limit, LimitCfg: cfgName}
	if zeroDisables && limit == 0 {
		c.Disabled = true
		return c
	}
	c.Tripped = value > limit
	return c
}

func finish(ch *Check, verdict bool, err error) {
	for _, c := range ch.Conditions {
		ch.Tripped = ch.Tripped || c.Tripped
	}
	if err != nil {
		ch.UpstreamError = err.Error()
		return
	}
	ch.Upstream = &verdict
	if verdict != ch.Tripped {
		ch.Note = "this breakdown disagrees with the upstream check; the inspection queries may have drifted from market/backpressure/backpressure.go"
	}
}

// Inspect computes the backpressure breakdown for the given ingest config.
func Inspect(ctx context.Context, cfg *config.CurioIngestConfig, db *harmonydb.DB) (*Report, error) {
	r := &Report{Snap: cfg.DoSnap}
	tmp := &CachedBackPressure{}

	sector := Check{Name: "sector"}
	if cfg.DoSnap {
		var encode, prove, waitDeal int64
		err := db.QueryRow(ctx, `
		WITH BufferedEncode AS (
			SELECT COUNT(p.task_id_encode) - COUNT(t.owner_id) AS buffered_encode
			FROM sectors_snap_pipeline p
			LEFT JOIN harmony_task t ON p.task_id_encode = t.id
			WHERE p.after_encode = false
		),
		BufferedProve AS (
			SELECT COUNT(p.task_id_prove) - COUNT(t.owner_id) AS buffered_prove
			FROM sectors_snap_pipeline p
			LEFT JOIN harmony_task t ON p.task_id_prove = t.id
			WHERE p.after_prove = true AND p.after_move_storage = false
		),
		WaitDealSectors AS (
			SELECT COUNT(DISTINCT osp.sector_number) AS wait_deal_sectors_count
			FROM open_sector_pieces osp
			LEFT JOIN sectors_snap_initial_pieces sip ON osp.sector_number = sip.sector_number
			WHERE sip.sector_number IS NULL
		)
		SELECT (SELECT buffered_encode FROM BufferedEncode), (SELECT buffered_prove FROM BufferedProve),
		       (SELECT wait_deal_sectors_count FROM WaitDealSectors)`).Scan(&encode, &prove, &waitDeal)
		if err != nil {
			return nil, xerrors.Errorf("counting buffered snap sectors: %w", err)
		}
		sector.Conditions = append(sector.Conditions,
			cond("snap: open deal sectors waiting for data", "Ingest.MaxQueueDealSector", waitDeal, int64(cfg.MaxQueueDealSector.Get()), true),
			cond("snap: encode tasks queued (unclaimed)", "Ingest.MaxQueueSnapEncode", encode, int64(cfg.MaxQueueSnapEncode.Get()), true),
			cond("snap: prove tasks queued (unclaimed)", "Ingest.MaxQueueSnapProve", prove, int64(cfg.MaxQueueSnapProve.Get()), true),
		)
	}
	var sdr, trees, porep, waitDeal int64
	err := db.QueryRow(ctx, `
		WITH BufferedSDR AS (
			SELECT COUNT(p.task_id_sdr) - COUNT(t.owner_id) AS buffered_sdr_count
			FROM sectors_sdr_pipeline p
			LEFT JOIN harmony_task t ON p.task_id_sdr = t.id
			WHERE p.after_sdr = false
		),
		BufferedTrees AS (
			SELECT COUNT(p.task_id_tree_r) - COUNT(t.owner_id) AS buffered_trees_count
			FROM sectors_sdr_pipeline p
			LEFT JOIN harmony_task t ON p.task_id_tree_r = t.id
			WHERE p.after_sdr = true AND p.after_tree_r = false
		),
		BufferedPoRep AS (
			SELECT COUNT(p.task_id_porep) - COUNT(t.owner_id) AS buffered_porep_count
			FROM sectors_sdr_pipeline p
			LEFT JOIN harmony_task t ON p.task_id_porep = t.id
			WHERE p.after_tree_r = true AND p.after_porep = false
		),
		WaitDealSectors AS (
			SELECT COUNT(DISTINCT osp.sector_number) AS wait_deal_sectors_count
			FROM open_sector_pieces osp
			LEFT JOIN sectors_sdr_initial_pieces sip ON osp.sector_number = sip.sector_number
			WHERE sip.sector_number IS NULL
		)
		SELECT (SELECT buffered_sdr_count FROM BufferedSDR), (SELECT buffered_trees_count FROM BufferedTrees),
		       (SELECT buffered_porep_count FROM BufferedPoRep), (SELECT wait_deal_sectors_count FROM WaitDealSectors)`).Scan(&sdr, &trees, &porep, &waitDeal)
	if err != nil {
		return nil, xerrors.Errorf("counting buffered sectors: %w", err)
	}
	sdrCond := cond("SDR tasks queued (unclaimed; includes batch-sealing sectors waiting for a batch task)", "Ingest.MaxQueueSDR", sdr, int64(cfg.MaxQueueSDR.Get()), false)
	if cfg.MaxQueueSDR.Get() == 0 {
		sdrCond.Note = "MaxQueueSDR = 0 does not disable this check: any queued SDR task trips it"
	}
	sector.Conditions = append(sector.Conditions,
		cond("open deal sectors waiting for data", "Ingest.MaxQueueDealSector", waitDeal, int64(cfg.MaxQueueDealSector.Get()), true),
		sdrCond,
		cond("tree tasks queued (unclaimed)", "Ingest.MaxQueueTrees", trees, int64(cfg.MaxQueueTrees.Get()), true),
		cond("PoRep tasks queued (unclaimed)", "Ingest.MaxQueuePoRep", porep, int64(cfg.MaxQueuePoRep.Get()), true),
	)
	v, err := tmp.checkSectorBackpressure(ctx, cfg, db)
	finish(&sector, v, err)

	mk12 := Check{Name: "mk12"}
	var running, downloading, verify int64
	err = db.QueryRow(ctx, `
		WITH pipeline_data AS (
			SELECT dp.uuid, dp.commp_task_id, dp.psd_task_id, dp.find_deal_task_id, dp.indexing_task_id,
			       pp.task_id AS downloading_task_id
			FROM market_mk12_deal_pipeline dp
			LEFT JOIN parked_pieces pp ON pp.piece_cid = dp.piece_cid
			WHERE dp.complete = false
		),
		joined AS (
			SELECT p.*, dt.owner_id AS downloading_owner, ct.owner_id AS commp_owner, pt.owner_id AS psd_owner,
			       ft.owner_id AS find_deal_owner, it.owner_id AS index_owner
			FROM pipeline_data p
			LEFT JOIN harmony_task dt ON dt.id = p.downloading_task_id
			LEFT JOIN harmony_task ct ON ct.id = p.commp_task_id
			LEFT JOIN harmony_task pt ON pt.id = p.psd_task_id
			LEFT JOIN harmony_task ft ON ft.id = p.find_deal_task_id
			LEFT JOIN harmony_task it ON it.id = p.indexing_task_id
		)
		SELECT
			COUNT(DISTINCT uuid) FILTER (
				WHERE (downloading_task_id IS NOT NULL AND downloading_owner IS NOT NULL)
				   OR (commp_task_id IS NOT NULL AND commp_owner IS NOT NULL)
				   OR (psd_task_id IS NOT NULL AND psd_owner IS NOT NULL)
				   OR (find_deal_task_id IS NOT NULL AND find_deal_owner IS NOT NULL)
				   OR (indexing_task_id IS NOT NULL AND index_owner IS NOT NULL)),
			COUNT(*) FILTER (WHERE downloading_task_id IS NOT NULL AND downloading_owner IS NULL),
			COUNT(*) FILTER (WHERE commp_task_id IS NOT NULL AND commp_owner IS NULL)
		FROM joined`).Scan(&running, &downloading, &verify)
	if err != nil {
		return nil, xerrors.Errorf("counting mk12 pipelines: %w", err)
	}
	mk12.Conditions = []Condition{
		cond("deal pipelines with a running task", "Ingest.MaxMarketRunningPipelines", running, int64(cfg.MaxMarketRunningPipelines.Get()), true),
		cond("downloads queued (unclaimed)", "Ingest.MaxQueueDownload", downloading, int64(cfg.MaxQueueDownload.Get()), true),
		cond("CommP tasks queued (unclaimed)", "Ingest.MaxQueueCommP", verify, int64(cfg.MaxQueueCommP.Get()), true),
	}
	v, err = tmp.checkMK12Backpressure(ctx, cfg, db)
	finish(&mk12, v, err)

	mk20 := Check{Name: "mk20"}
	var running20, downloading20, commp20, agg20 int64
	err = db.QueryRow(ctx, `
		WITH pipeline_data AS (
			SELECT dp.id, dp.commp_task_id, dp.agg_task_id, dp.indexing_task_id, pp.task_id AS downloading_task_id
			FROM market_mk20_pipeline dp
			LEFT JOIN parked_pieces pp ON pp.piece_cid = dp.piece_cid AND pp.piece_padded_size = dp.piece_size
			WHERE dp.complete = false
		),
		joined AS (
			SELECT p.*, dt.owner_id AS downloading_owner, ct.owner_id AS commp_owner,
			       at.owner_id AS agg_owner, it.owner_id AS index_owner
			FROM pipeline_data p
			LEFT JOIN harmony_task dt ON dt.id = p.downloading_task_id
			LEFT JOIN harmony_task ct ON ct.id = p.commp_task_id
			LEFT JOIN harmony_task at ON at.id = p.agg_task_id
			LEFT JOIN harmony_task it ON it.id = p.indexing_task_id
		)
		SELECT
			COUNT(DISTINCT id) FILTER (
				WHERE (downloading_task_id IS NOT NULL AND downloading_owner IS NOT NULL)
				   OR (commp_task_id IS NOT NULL AND commp_owner IS NOT NULL)
				   OR (agg_task_id IS NOT NULL AND agg_owner IS NOT NULL)
				   OR (indexing_task_id IS NOT NULL AND index_owner IS NOT NULL)),
			COUNT(DISTINCT id) FILTER (WHERE downloading_task_id IS NOT NULL AND downloading_owner IS NULL),
			COUNT(DISTINCT id) FILTER (WHERE commp_task_id IS NOT NULL AND commp_owner IS NULL),
			COUNT(DISTINCT id) FILTER (WHERE agg_task_id IS NOT NULL AND agg_owner IS NULL)
		FROM joined`).Scan(&running20, &downloading20, &commp20, &agg20)
	if err != nil {
		return nil, xerrors.Errorf("counting mk20 pipelines: %w", err)
	}
	aggCond := Condition{Name: "aggregation tasks queued (not a backpressure condition upstream)", Value: agg20}
	mk20.Conditions = []Condition{
		cond("deal pipelines with a running task", "Ingest.MaxMarketRunningPipelines", running20, int64(cfg.MaxMarketRunningPipelines.Get()), true),
		cond("downloads queued (unclaimed)", "Ingest.MaxQueueDownload", downloading20, int64(cfg.MaxQueueDownload.Get()), true),
		cond("CommP tasks queued (unclaimed)", "Ingest.MaxQueueCommP", commp20, int64(cfg.MaxQueueCommP.Get()), true),
		aggCond,
	}
	v, err = tmp.checkMK20Backpressure(ctx, cfg, db)
	finish(&mk20, v, err)

	r.Checks = []Check{sector, mk12, mk20}
	r.MK12Busy = mk12.Tripped || sector.Tripped
	r.MK20Busy = mk20.Tripped || sector.Tripped

	auroraRegistry.mu.Lock()
	insts := append([]*auroraInstance(nil), auroraRegistry.insts...)
	auroraRegistry.mu.Unlock()
	for _, inst := range insts {
		items := inst.c.cache.GetItems()
		var cvs []CachedValue
		inst.mu.Lock()
		for key, name := range auroraKeyName {
			val, ok := items[key]
			if !ok {
				continue
			}
			b, _ := val.(bool)
			cv := CachedValue{Check: name, Value: b}
			if at, ok := inst.setAt[key]; ok {
				at := at
				cv.SetAt = &at
				cv.ExpiresIn = time.Until(at.Add(auroraKeyTTL[key])).Round(time.Second).String()
			}
			cvs = append(cvs, cv)
		}
		inst.mu.Unlock()
		r.Cached = append(r.Cached, cvs)
	}
	if len(insts) == 0 {
		r.Notes = append(r.Notes, "no market backpressure cache on this node: deals are not received here, or none arrived since startup")
	}
	for _, cvs := range r.Cached {
		for _, cv := range cvs {
			for _, ch := range r.Checks {
				if ch.Name == cv.Check && cv.Value != ch.Tripped {
					r.Notes = append(r.Notes, "cached "+cv.Check+" verdict differs from the fresh one; deals keep seeing the cached value until it expires ("+cv.ExpiresIn+")")
				}
			}
		}
	}
	return r, nil
}
