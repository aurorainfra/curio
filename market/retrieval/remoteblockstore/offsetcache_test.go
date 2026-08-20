package remoteblockstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/market/indexstore"
)

type fakeOffsetIdx struct {
	pieces map[string][]indexstore.BlockOffset
}

func (f *fakeOffsetIdx) GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid, maxEntries int) ([]indexstore.BlockOffset, error) {
	offs, ok := f.pieces[pieceCid.KeyString()]
	if !ok {
		return nil, nil
	}
	if len(offs) > maxEntries {
		return nil, indexstore.ErrPieceIndexTooLarge
	}
	return offs, nil
}

func testPiece(t *testing.T, seed string, entries int) (cid.Cid, []indexstore.BlockOffset) {
	h, err := multihash.Sum([]byte("piece-"+seed), multihash.SHA2_256, -1)
	require.NoError(t, err)
	pc := cid.NewCidV1(cid.FilCommitmentUnsealed, h)

	offs := make([]indexstore.BlockOffset, entries)
	for i := range offs {
		bh, err := multihash.Sum([]byte(fmt.Sprintf("blk-%s-%d", seed, i)), multihash.SHA2_256, -1)
		require.NoError(t, err)
		offs[i] = indexstore.BlockOffset{Hash: bh, Offset: uint64(i) * 1000}
	}
	return pc, offs
}

func waitCached(t *testing.T, oc *offsetCache, mh multihash.Multihash) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, ok := oc.lookup(context.Background(), string(mh)); ok {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestOffsetCachePolicy(t *testing.T) {
	fake := &fakeOffsetIdx{pieces: map[string][]indexstore.BlockOffset{}}

	oc := newOffsetCache(fake, 1) // 1 MiB budget
	// entry cost: 2*34+88 = 156B; 50-entry piece ≈ 7.8KB; budget/4 cap = 256KB
	// budget fits ~134 such pieces; shrink the budget for eviction testing
	oc.budget = 40 << 10 // 40KiB: fits 5 pieces of ~7.8KB, 6th needs eviction

	mkPiece := func(seed string) (cid.Cid, []indexstore.BlockOffset) {
		pc, offs := testPiece(t, seed, 50)
		fake.pieces[pc.KeyString()] = offs
		return pc, offs
	}

	// admit 5 pieces at the minimum score
	var firstOffs []indexstore.BlockOffset
	for i := 0; i < 5; i++ {
		pc, offs := mkPiece(fmt.Sprint(i))
		if i == 0 {
			firstOffs = offs
		}
		for j := 0; j < offsetCacheAdmitMin; j++ {
			oc.serveNotify(pc)
		}
		require.True(t, waitCached(t, oc, offs[0].Hash), "piece %d should be cached", i)
	}

	// entryLen comes from the next offset; last block has entryLen 0
	_, off, entryLen, ok := oc.lookup(context.Background(), string(firstOffs[3].Hash))
	require.True(t, ok)
	require.Equal(t, uint64(3000), off)
	require.Equal(t, uint64(1000), entryLen)
	_, _, entryLen, ok = oc.lookup(context.Background(), string(firstOffs[49].Hash))
	require.True(t, ok)
	require.Equal(t, uint64(0), entryLen)

	// a low-score candidate must NOT displace anything (pre-check reject:
	// budget is full and it does not beat the weakest by the margin)
	weak, weakOffs := mkPiece("weak")
	for j := 0; j < offsetCacheAdmitMin; j++ {
		oc.serveNotify(weak)
	}
	require.False(t, waitCached(t, oc, weakOffs[0].Hash), "weak candidate should not be admitted")

	// a high-score candidate displaces exactly one low-score piece
	strong, strongOffs := mkPiece("strong")
	for j := 0; j < 100; j++ {
		oc.serveNotify(strong)
	}
	require.True(t, waitCached(t, oc, strongOffs[0].Hash), "strong candidate should be admitted")

	oc.admin.Lock()
	npieces := len(oc.pieces)
	_, strongCached := oc.pieces[strong.KeyString()]
	oc.admin.Unlock()
	require.True(t, strongCached)
	require.Equal(t, 5, npieces, "one piece should have been evicted to fit the strong candidate")

	// pieces refused as too large go on cooldown and are not cached
	big, bigOffs := testPiece(t, "big", 60)
	fake.pieces[big.KeyString()] = bigOffs
	oc.maxEntries = 55
	for j := 0; j < 100; j++ {
		oc.serveNotify(big)
	}
	require.False(t, waitCached(t, oc, bigOffs[0].Hash), "too-large piece should be refused")
	oc.admin.Lock()
	_, onCooldown := oc.cooldown[big.KeyString()]
	oc.admin.Unlock()
	require.True(t, onCooldown)
}
