package slotmgr

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"
)

// newTestSlotMgr builds a SlotMgr without a database; only the in-memory
// paths (Get, AbortSlot, Available, EnableTrim) are usable.
func newTestSlotMgr(offs []uint64, occupied map[uint64][]abi.SectorID) *SlotMgr {
	sm := &SlotMgr{}
	for _, off := range offs {
		st := &slot{slotOffset: off, sectors: map[abi.SectorID]struct{}{}}
		for _, id := range occupied[off] {
			st.sectors[id] = struct{}{}
		}
		sm.slots = append(sm.slots, st)
	}
	sm.cond = sync.NewCond(&sm.lk)
	return sm
}

// blockingTrimmer records trim calls and blocks each one until released.
type blockingTrimmer struct {
	calls   chan uint64
	release chan struct{}
}

func newBlockingTrimmer() *blockingTrimmer {
	return &blockingTrimmer{calls: make(chan uint64, 16), release: make(chan struct{})}
}

func (b *blockingTrimmer) trim(off uint64) error {
	b.calls <- off
	<-b.release
	return nil
}

func (b *blockingTrimmer) expectCall(t *testing.T, off uint64) {
	t.Helper()
	select {
	case got := <-b.calls:
		require.Equal(t, off, got)
	case <-time.After(5 * time.Second):
		t.Fatalf("expected trim of slot %d", off)
	}
}

func (b *blockingTrimmer) expectNoCall(t *testing.T) {
	t.Helper()
	select {
	case got := <-b.calls:
		t.Fatalf("unexpected trim of slot %d", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func getAsync(sm *SlotMgr, ids []abi.SectorID) <-chan uint64 {
	ch := make(chan uint64, 1)
	go func() { ch <- sm.Get(ids) }()
	return ch
}

var testSector = abi.SectorID{Miner: 1000, Number: 1}

func TestStartupTrimOnlyFreeSlots(t *testing.T) {
	// slot 0 holds a sector waiting for C1/finalize, slot 100 is free
	sm := newTestSlotMgr([]uint64{0, 100}, map[uint64][]abi.SectorID{0: {testSector}})
	tr := newBlockingTrimmer()

	sm.EnableTrim(tr.trim)
	tr.expectCall(t, 100)
	tr.expectNoCall(t)

	// Nothing is available while the only free slot is being trimmed
	require.Equal(t, 0, sm.Available())

	got := getAsync(sm, []abi.SectorID{{Miner: 1000, Number: 2}})
	select {
	case off := <-got:
		t.Fatalf("Get returned slot %d during trim", off)
	case <-time.After(100 * time.Millisecond):
	}

	close(tr.release)
	select {
	case off := <-got:
		require.Equal(t, uint64(100), off)
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not return after trim finished")
	}
}

func TestAbortTrimsSlotBeforeReuse(t *testing.T) {
	sm := newTestSlotMgr([]uint64{0}, nil)
	tr := newBlockingTrimmer()

	sm.EnableTrim(tr.trim)
	tr.expectCall(t, 0)
	tr.release <- struct{}{}
	require.Eventually(t, func() bool { return sm.Available() == 1 }, 5*time.Second, 10*time.Millisecond)

	off := sm.Get([]abi.SectorID{testSector})
	require.Equal(t, uint64(0), off)
	tr.expectNoCall(t)

	require.NoError(t, sm.AbortSlot(off))
	tr.expectCall(t, 0)
	require.Equal(t, 0, sm.Available())

	tr.release <- struct{}{}
	require.Eventually(t, func() bool { return sm.Available() == 1 }, 5*time.Second, 10*time.Millisecond)
}

func TestTrimNotRestartedWhileRunning(t *testing.T) {
	sm := newTestSlotMgr([]uint64{0}, nil)
	tr := newBlockingTrimmer()

	sm.EnableTrim(tr.trim)
	tr.expectCall(t, 0)

	sm.lk.Lock()
	sm.slotFreedLocked(sm.slots[0], "test")
	sm.lk.Unlock()
	tr.expectNoCall(t)

	close(tr.release)
	require.Eventually(t, func() bool { return sm.Available() == 1 }, 5*time.Second, 10*time.Millisecond)
}

func TestNoTrimConfigured(t *testing.T) {
	sm := newTestSlotMgr([]uint64{0}, nil)

	off := sm.Get([]abi.SectorID{testSector})
	require.Equal(t, 0, sm.Available())
	require.NoError(t, sm.AbortSlot(off))
	require.Equal(t, 1, sm.Available())
}
