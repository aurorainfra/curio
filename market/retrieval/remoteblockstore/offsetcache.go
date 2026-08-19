package remoteblockstore

import (
	"context"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/curio/market/indexstore"
)

// offsetCache keeps the full block index (multihash -> offset) of recently
// served pieces in memory, so block retrieval can skip both index lookups
// (PiecesContainingMultihash and GetOffset) for blocks of hot pieces. Piece
// indexes are loaded asynchronously after a piece first serves a block; the
// cache is bounded to a configured number of pieces (LRU), and a global
// reverse map resolves multihash -> (piece, position) for cached pieces.
//
// A ~32GiB piece with ~1MiB blocks has ~32k entries (~2-3MB with the reverse
// map), so the memory bound is roughly pieces * 3MB.
type offsetCache struct {
	idx interface {
		GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid) ([]indexstore.BlockOffset, error)
	}

	mu      sync.Mutex
	rev     map[string]revEntry // multihash bytes -> cached piece position
	loading map[string]struct{} // piece keys currently being loaded
	pieces  *lru.Cache[string, *pieceOffsets]
}

type pieceOffsets struct {
	piece cid.Cid
	mhs   []string // multihash bytes, sorted by offset
	offs  []uint64
}

type revEntry struct {
	po  *pieceOffsets
	idx int
}

func newOffsetCache(idx interface {
	GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid) ([]indexstore.BlockOffset, error)
}, pieces int) *offsetCache {
	oc := &offsetCache{
		idx:     idx,
		rev:     map[string]revEntry{},
		loading: map[string]struct{}{},
	}
	// the evict callback runs while oc.mu is held: every Add/Remove on the
	// LRU happens under the lock
	oc.pieces, _ = lru.NewWithEvict[string, *pieceOffsets](pieces, oc.evicted)
	return oc
}

// evicted drops the reverse map entries of an evicted piece. Called with
// oc.mu held (all LRU mutations happen under it).
func (oc *offsetCache) evicted(_ string, po *pieceOffsets) {
	for _, m := range po.mhs {
		if e, ok := oc.rev[m]; ok && e.po == po {
			delete(oc.rev, m)
		}
	}
}

// lookup resolves a multihash to (piece, offset, entryLen). entryLen is the
// exact CAR entry length derived from the next block's offset, or 0 for the
// last block of a piece (caller bounds the read by the piece size).
func (oc *offsetCache) lookup(mh string) (piece cid.Cid, offset uint64, entryLen uint64, ok bool) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	e, found := oc.rev[mh]
	if !found {
		return cid.Undef, 0, 0, false
	}

	// bump recency
	_, _ = oc.pieces.Get(e.po.piece.KeyString())

	offset = e.po.offs[e.idx]
	if e.idx+1 < len(e.po.offs) {
		entryLen = e.po.offs[e.idx+1] - offset
	}
	return e.po.piece, offset, entryLen, true
}

// maybeLoad asynchronously loads a piece's block index into the cache unless
// it is already cached or being loaded.
func (oc *offsetCache) maybeLoad(piece cid.Cid) {
	key := piece.KeyString()

	oc.mu.Lock()
	if _, ok := oc.loading[key]; ok {
		oc.mu.Unlock()
		return
	}
	if oc.pieces.Contains(key) {
		oc.mu.Unlock()
		return
	}
	oc.loading[key] = struct{}{}
	oc.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		start := time.Now()
		offs, err := oc.idx.GetPieceBlockOffsets(ctx, piece)

		oc.mu.Lock()
		defer oc.mu.Unlock()
		delete(oc.loading, key)

		if err != nil {
			log.Warnw("loading piece block offsets failed", "piece", piece, "err", err)
			return
		}
		if len(offs) == 0 {
			return
		}

		po := &pieceOffsets{
			piece: piece,
			mhs:   make([]string, len(offs)),
			offs:  make([]uint64, len(offs)),
		}
		for i, o := range offs {
			po.mhs[i] = string(o.Hash)
			po.offs[i] = o.Offset
		}

		oc.pieces.Add(key, po)
		for i, m := range po.mhs {
			oc.rev[m] = revEntry{po: po, idx: i}
		}

		log.Debugw("cached piece block offsets", "piece", piece, "blocks", len(offs), "took", time.Since(start))
	}()
}

// invalidate drops a piece from the cache (e.g. after a read through cached
// offsets failed).
func (oc *offsetCache) invalidate(piece cid.Cid) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	oc.pieces.Remove(piece.KeyString()) // evict callback cleans the reverse map
}
