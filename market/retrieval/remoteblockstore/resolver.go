package remoteblockstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/yugabyte/gocql"
	"go.opencensus.io/stats"
)

// BlockLoc locates one block inside a piece for bulk retrieval. Exactly one
// of EntryLen / BlockSize may be known:
//
//   - EntryLen is the exact CAR entry length (varint + cid + payload),
//     known when the location came from the in-memory offset cache; 0 with
//     Offset>0 can also mean "last entry of the piece" (bound by piece size).
//   - BlockSize is the payload size from the block index, known when the
//     piece was resolved through PiecesContainingMultihash; the CAR entry is
//     at most BlockSize+MaxCarBlockPrefixSize bytes.
//
// Both zero means only the offset is known (hinted resolve) and readers
// must bound the entry by the piece size or a protocol cap.
type BlockLoc struct {
	Piece     cid.Cid
	Offset    uint64
	EntryLen  uint64
	BlockSize uint64

	// Alts are other pieces that also contain the block (only populated on
	// un-hinted resolves). Callers can re-resolve against an alt when the
	// primary piece has no readable unsealed copy.
	Alts []cid.Cid
}

// ResolveBlock resolves a block cid to its piece location without reading
// any data. Resolution order: in-memory offset cache (no index queries),
// then the hinted piece (one offset query), then the full block index.
// Hints are advisory: a hint that does not resolve — stale, or a
// different piece-cid form than the index key (e.g. the deal's v1 commP
// where the index is keyed by v2) — costs one extra lookup, never a
// not-found. Only blocks absent from the index return format.ErrNotFound.
func (ro *RemoteBlockstore) ResolveBlock(ctx context.Context, c cid.Cid, pieceHint cid.Cid) (BlockLoc, error) {
	mh := c.Hash()

	// Offset-cache hit is authoritative even under a differing hint: any
	// piece copy of a content-addressed block serves equally, and reads are
	// hash-verified downstream.
	if ro.offsets != nil {
		if piece, offset, entryLen, ok := ro.offsets.lookup(ctx, string(mh)); ok {
			return BlockLoc{Piece: piece, Offset: offset, EntryLen: entryLen}, nil
		}
	}

	if pieceHint.Defined() {
		offset, err := ro.idxApi.GetOffset(ctx, pieceHint, mh)
		if err == nil {
			return BlockLoc{Piece: pieceHint, Offset: offset}, nil
		}
		if !errors.Is(err, gocql.ErrNotFound) {
			return BlockLoc{}, fmt.Errorf("getting offset for cid %s in hinted piece %s: %w", c, pieceHint, err)
		}
		stats.Record(ctx, BulkHintMissCount.M(1))
	}

	loc := BlockLoc{}
	pieces, err := ro.idxApi.PiecesContainingMultihash(ctx, mh)
	if err != nil {
		return BlockLoc{}, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	if len(pieces) == 0 {
		return BlockLoc{}, format.ErrNotFound{Cid: c}
	}
	loc.Piece = pieces[0].PieceCid
	loc.BlockSize = pieces[0].BlockSize
	for _, p := range pieces[1:] {
		loc.Alts = append(loc.Alts, p.PieceCid)
	}

	offset, err := ro.idxApi.GetOffset(ctx, loc.Piece, mh)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return BlockLoc{}, format.ErrNotFound{Cid: c}
		}
		return BlockLoc{}, fmt.Errorf("getting offset for cid %s in piece %s: %w", c, loc.Piece, err)
	}
	loc.Offset = offset

	return loc, nil
}

// BulkServeNotify records that blocksServed blocks of piece were served by
// the bulk endpoint, feeding the same offset-cache admission scoring as
// per-block serves on the regular path (one call per piece per request
// instead of one per block).
func (ro *RemoteBlockstore) BulkServeNotify(piece cid.Cid, blocksServed int64) {
	if ro.offsets == nil || blocksServed <= 0 {
		return
	}
	ro.offsets.serveNotifyN(piece, blocksServed)
}

// BulkInvalidate drops a piece's cached offsets after a read-through
// failure (stale index self-healing, same as the single-block path).
func (ro *RemoteBlockstore) BulkInvalidate(piece cid.Cid) {
	if ro.offsets == nil {
		return
	}
	ro.offsets.invalidate(piece)
}
