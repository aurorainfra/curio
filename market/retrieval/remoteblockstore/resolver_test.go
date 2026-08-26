package remoteblockstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/gocql"

	"github.com/filecoin-project/curio/market/indexstore"
)

// fakeResolverIdx implements the full idxAPI over in-memory piece indexes.
type fakeResolverIdx struct {
	fakeOffsetIdx // GetPieceBlockOffsets over .pieces

	// containing maps multihash -> ordered piece list
	containing map[string][]indexstore.PieceInfo

	containingCalls atomic.Int64
	offsetCalls     atomic.Int64
}

func (f *fakeResolverIdx) PiecesContainingMultihash(ctx context.Context, m multihash.Multihash) ([]indexstore.PieceInfo, error) {
	f.containingCalls.Add(1)
	return f.containing[string(m)], nil
}

func (f *fakeResolverIdx) GetOffset(ctx context.Context, pieceCid cid.Cid, hash multihash.Multihash) (uint64, error) {
	f.offsetCalls.Add(1)
	for _, bo := range f.pieces[pieceCid.KeyString()] {
		if string(bo.Hash) == string(hash) {
			return bo.Offset, nil
		}
	}
	return 0, fmt.Errorf("getting offset for piece %s: %w", pieceCid, gocql.ErrNotFound)
}

func TestResolveBlock(t *testing.T) {
	ctx := context.Background()

	p1, offs1 := testPiece(t, "r1", 10)
	p2, offs2 := testPiece(t, "r2", 10)

	fake := &fakeResolverIdx{
		fakeOffsetIdx: fakeOffsetIdx{pieces: map[string][]indexstore.BlockOffset{
			p1.KeyString(): offs1,
			p2.KeyString(): offs2,
		}},
		containing: map[string][]indexstore.PieceInfo{},
	}
	// block 3 of p1 is also in p2 (same content in two pieces, at a
	// different offset there)
	offs2[3] = indexstore.BlockOffset{Hash: offs1[3].Hash, Offset: 7777}
	fake.containing[string(offs1[3].Hash)] = []indexstore.PieceInfo{
		{PieceCid: p1, BlockSize: 900},
		{PieceCid: p2, BlockSize: 900},
	}

	ro := &RemoteBlockstore{idxApi: fake}
	blk3 := cid.NewCidV1(cid.Raw, offs1[3].Hash)

	t.Run("unhinted", func(t *testing.T) {
		loc, err := ro.ResolveBlock(ctx, blk3, cid.Undef)
		require.NoError(t, err)
		require.Equal(t, p1, loc.Piece)
		require.Equal(t, offs1[3].Offset, loc.Offset)
		require.Equal(t, uint64(900), loc.BlockSize)
		require.Equal(t, []cid.Cid{p2}, loc.Alts)
	})

	t.Run("hinted skips piece lookup", func(t *testing.T) {
		before := fake.containingCalls.Load()
		loc, err := ro.ResolveBlock(ctx, blk3, p2)
		require.NoError(t, err)
		require.Equal(t, p2, loc.Piece)
		require.Equal(t, offs2[3].Offset, loc.Offset) // offset differs per piece
		require.Zero(t, loc.BlockSize)
		require.Empty(t, loc.Alts)
		require.Equal(t, before, fake.containingCalls.Load())
	})

	t.Run("not indexed", func(t *testing.T) {
		unknown := cid.NewCidV1(cid.Raw, offs2[7].Hash) // not in containing map
		_, err := ro.ResolveBlock(ctx, unknown, cid.Undef)
		require.True(t, format.IsNotFound(err))
	})

	t.Run("hinted, not in hinted piece", func(t *testing.T) {
		other := cid.NewCidV1(cid.Raw, offs2[7].Hash)
		_, err := ro.ResolveBlock(ctx, other, p1)
		require.True(t, format.IsNotFound(err))
	})
}

func TestResolveBlockOffsetCache(t *testing.T) {
	ctx := context.Background()

	p1, offs1 := testPiece(t, "rc1", 10)
	fake := &fakeResolverIdx{
		fakeOffsetIdx: fakeOffsetIdx{pieces: map[string][]indexstore.BlockOffset{
			p1.KeyString(): offs1,
		}},
		containing: map[string][]indexstore.PieceInfo{},
	}

	ro := &RemoteBlockstore{idxApi: fake, offsets: newOffsetCache(fake, 1)}

	// admit the piece via bulk-notify weight (one call, block count as n)
	ro.BulkServeNotify(p1, offsetCacheAdmitMin)
	require.True(t, waitCached(t, ro.offsets, offs1[0].Hash))

	before := fake.offsetCalls.Load()
	loc, err := ro.ResolveBlock(ctx, cid.NewCidV1(cid.Raw, offs1[4].Hash), cid.Undef)
	require.NoError(t, err)
	require.Equal(t, p1, loc.Piece)
	require.Equal(t, offs1[4].Offset, loc.Offset)
	// exact entry length from the neighbour offset (testPiece spaces by 1000)
	require.Equal(t, uint64(1000), loc.EntryLen)
	require.Equal(t, before, fake.offsetCalls.Load()) // no index queries

	// invalidate drops back to the index path (block not in the containing
	// map -> NotFound via PiecesContainingMultihash, not the cache)
	ro.BulkInvalidate(p1)
	beforeContaining := fake.containingCalls.Load()
	_, err = ro.ResolveBlock(ctx, cid.NewCidV1(cid.Raw, offs1[4].Hash), cid.Undef)
	require.True(t, format.IsNotFound(err))
	require.Greater(t, fake.containingCalls.Load(), beforeContaining)
}
