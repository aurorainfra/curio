package remoteblockstore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"go.opencensus.io/stats"

	"github.com/filecoin-project/curio/market/indexstore"
)

// offsetCache keeps the full block index (multihash -> offset) of hot pieces
// in memory, so block retrieval can skip both per-block index lookups
// (PiecesContainingMultihash and GetOffset). It is bounded by a byte budget
// (Indexing.RetrievalOffsetCacheMemMiB).
//
// Loading a piece index is expensive (hundreds of ms to seconds at ~100k
// entries/s from Cassandra, and pieces range from ~32k entries to tens of
// millions), so admission is competitive and rate-based:
//
//   - every piece — cached or not — has a windowed access counter (two
//     buckets rotated every offsetCacheWindow; score = accesses over the
//     last 1-2 windows);
//   - a candidate is considered once its score reaches offsetCacheAdmitMin;
//   - if the loaded index does not fit in free budget, the lowest-score
//     cached pieces are displaced only when the candidate's score beats
//     their combined score by offsetCacheEvictPct percent plus
//     offsetCacheAdmitMin — new pieces must demonstrate meaningfully more
//     traffic than what they evict, which is the churn brake;
//   - pieces that fail to load, exceed the size caps, or lose the
//     comparison are not reconsidered for offsetCacheCooldown.
//
// Index loads are asynchronous and deduplicated; requests never wait on a
// load — while an index is loading (or was refused), blocks of that piece
// are served through the regular single-lookup path.
const (
	// offsetCacheWindow is the access-counter rotation period.
	offsetCacheWindow = time.Minute
	// offsetCacheAdmitMin is the minimum windowed access score before a
	// piece is considered for admission, and the absolute term of the
	// eviction margin.
	offsetCacheAdmitMin = 16
	// offsetCacheEvictPct is the relative eviction margin: a candidate must
	// have this percent more windowed accesses than the pieces it displaces.
	offsetCacheEvictPct = 25
	// offsetCacheCooldown is how long a failed/refused/outcompeted piece is
	// ignored before it can be considered again.
	offsetCacheCooldown = 15 * time.Minute
	// offsetCacheMaxCandidates bounds the candidate counter map.
	offsetCacheMaxCandidates = 8192
	// offsetCachePieceBudgetDiv caps a single piece index at
	// budget/offsetCachePieceBudgetDiv bytes.
	offsetCachePieceBudgetDiv = 4
	// offsetCacheEntryOverhead approximates per-entry bookkeeping bytes on
	// top of twice the multihash length (index slices + reverse map entry).
	offsetCacheEntryOverhead = 88
	// offsetCacheFullWatermarkDiv: below budget/this of free space the cache
	// is considered full and admission requires beating the eviction margin
	// up front, before paying for the index load.
	offsetCacheFullWatermarkDiv = 16
	// offsetCacheAvgEntryBytes estimates a sha2-256 entry's cache cost; used
	// to derive the entry-count bound handed to GetPieceBlockOffsets.
	offsetCacheAvgEntryBytes = 34*2 + offsetCacheEntryOverhead
	// offsetCacheMaxEntriesCap is the absolute per-piece entry bound.
	offsetCacheMaxEntriesCap = 1_000_000

	offsetCacheShards = 64
)

// windowCounter is a two-bucket windowed access counter.
type windowCounter struct{ cur, prev atomic.Int64 }

func (w *windowCounter) bump()        { w.cur.Add(1) }
func (w *windowCounter) score() int64 { return w.cur.Load() + w.prev.Load() }
func (w *windowCounter) rotate()      { w.prev.Store(w.cur.Swap(0)) }

type pieceOffsets struct {
	piece cid.Cid
	mhs   []string // multihash bytes, sorted by offset
	offs  []uint64
	bytes int64 // accounted cache cost
	hits  windowCounter
}

type revEntry struct {
	po  *pieceOffsets
	idx int32
}

type revShard struct {
	mu sync.RWMutex
	m  map[string]revEntry
}

type offsetCache struct {
	idx interface {
		GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid, maxEntries int) ([]indexstore.BlockOffset, error)
	}

	budget     int64
	maxEntries int

	// sharded multihash -> (piece, position) reverse index; the only state
	// touched on the lookup hot path
	shards [offsetCacheShards]revShard

	// admin guards everything below (admissions, evictions, accounting);
	// it is never taken on the lookup path. Write hold times are recorded
	// as OffsetCacheAdminLockHoldMs.
	admin      sync.Mutex
	pieces     map[string]*pieceOffsets
	used       int64
	loading    map[string]struct{}
	cooldown   map[string]time.Time
	candidates map[string]*windowCounter
}

func newOffsetCache(idx interface {
	GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid, maxEntries int) ([]indexstore.BlockOffset, error)
}, memMiB int) *offsetCache {
	oc := &offsetCache{
		idx:        idx,
		budget:     int64(memMiB) << 20,
		pieces:     map[string]*pieceOffsets{},
		loading:    map[string]struct{}{},
		cooldown:   map[string]time.Time{},
		candidates: map[string]*windowCounter{},
	}

	oc.maxEntries = int(oc.budget / offsetCachePieceBudgetDiv / offsetCacheAvgEntryBytes)
	if oc.maxEntries > offsetCacheMaxEntriesCap {
		oc.maxEntries = offsetCacheMaxEntriesCap
	}
	if oc.maxEntries < 1024 {
		oc.maxEntries = 1024
	}

	for i := range oc.shards {
		oc.shards[i].m = map[string]revEntry{}
	}

	go oc.rotateLoop()
	return oc
}

func (oc *offsetCache) shard(mh string) *revShard {
	return &oc.shards[mh[len(mh)-1]%offsetCacheShards]
}

// lookup resolves a multihash to (piece, offset, entryLen) from memory.
// entryLen is the exact CAR entry length derived from the next block's
// offset, or 0 for the last block of a piece. Hot path: one shard RLock and
// atomic counter updates, no global locks.
func (oc *offsetCache) lookup(ctx context.Context, mh string) (piece cid.Cid, offset uint64, entryLen uint64, ok bool) {
	sh := oc.shard(mh)
	sh.mu.RLock()
	e, found := sh.m[mh]
	sh.mu.RUnlock()

	if !found {
		stats.Record(ctx, OffsetCacheMissCount.M(1))
		return cid.Undef, 0, 0, false
	}

	e.po.hits.bump()
	stats.Record(ctx, OffsetCacheHitCount.M(1))

	offset = e.po.offs[e.idx]
	if int(e.idx)+1 < len(e.po.offs) {
		entryLen = e.po.offs[e.idx+1] - offset
	}
	return e.po.piece, offset, entryLen, true
}

// serveNotify records that a block of piece was served via the regular
// (index-lookup) path, and may trigger an asynchronous index load once the
// piece's windowed access rate justifies it. It never blocks the calling
// request on loading.
func (oc *offsetCache) serveNotify(piece cid.Cid) {
	key := piece.KeyString()
	now := time.Now()

	lockStart := time.Now()
	oc.admin.Lock()

	if po, ok := oc.pieces[key]; ok {
		oc.admin.Unlock()
		// accesses count towards retention regardless of serve path
		po.hits.bump()
		return
	}
	if _, ok := oc.loading[key]; ok {
		oc.admin.Unlock()
		return
	}
	if until, ok := oc.cooldown[key]; ok && now.Before(until) {
		oc.admin.Unlock()
		return
	}

	wc, ok := oc.candidates[key]
	if !ok {
		if len(oc.candidates) >= offsetCacheMaxCandidates {
			oc.admin.Unlock()
			return
		}
		wc = &windowCounter{}
		oc.candidates[key] = wc
	}
	wc.bump()
	score := wc.score()

	if score < offsetCacheAdmitMin {
		oc.admin.Unlock()
		return
	}

	// When the cache is nearly full a load will almost certainly require an
	// eviction, so require the candidate to beat the weakest cached piece by
	// the eviction margin *before* paying for the expensive index load —
	// loads triggered under pressure then near-certainly win the post-load
	// comparison instead of getting dropped onto cooldown mid-ramp. With
	// room available, candidates load as soon as they qualify.
	if len(oc.pieces) > 0 && oc.budget-oc.used < oc.budget/offsetCacheFullWatermarkDiv {
		weakest := int64(-1)
		for _, po := range oc.pieces {
			if s := po.hits.score(); weakest == -1 || s < weakest {
				weakest = s
			}
		}
		if score <= weakest+(weakest*offsetCacheEvictPct)/100+offsetCacheAdmitMin {
			oc.admin.Unlock()
			return
		}
	}

	oc.loading[key] = struct{}{}
	oc.admin.Unlock()
	stats.Record(context.Background(), OffsetCacheAdminLockHoldMs.M(float64(time.Since(lockStart).Microseconds())/1000))

	stats.Record(context.Background(), OffsetCacheLoadCount.M(1))
	go oc.load(piece, key, score)
}

func (oc *offsetCache) load(piece cid.Cid, key string, score int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	offs, err := oc.idx.GetPieceBlockOffsets(ctx, piece, oc.maxEntries)
	stats.Record(context.Background(), OffsetCacheLoadDurationMs.M(float64(time.Since(start).Milliseconds())))

	if err != nil || len(offs) == 0 {
		switch {
		case errors.Is(err, indexstore.ErrPieceIndexTooLarge):
			stats.Record(context.Background(), OffsetCacheRefusedLargeCount.M(1))
			log.Debugw("offset cache: piece index too large", "piece", piece, "maxEntries", oc.maxEntries)
		case err != nil:
			stats.Record(context.Background(), OffsetCacheLoadErrorCount.M(1))
			log.Warnw("offset cache: loading piece index failed", "piece", piece, "err", err)
		}

		oc.admin.Lock()
		delete(oc.loading, key)
		delete(oc.candidates, key)
		oc.cooldown[key] = time.Now().Add(offsetCacheCooldown)
		oc.admin.Unlock()
		return
	}

	po := &pieceOffsets{
		piece: piece,
		mhs:   make([]string, len(offs)),
		offs:  make([]uint64, len(offs)),
	}
	var cost int64
	for i, o := range offs {
		po.mhs[i] = string(o.Hash)
		po.offs[i] = o.Offset
		cost += int64(2*len(o.Hash) + offsetCacheEntryOverhead)
	}
	po.bytes = cost
	// carry the candidate's momentum so a fresh admit isn't instantly the
	// weakest piece in the next comparison
	po.hits.cur.Store(score)

	var evicted []*pieceOffsets

	lockStart := time.Now()
	oc.admin.Lock()
	delete(oc.loading, key)
	delete(oc.candidates, key)

	admit := true
	switch {
	case cost > oc.budget/offsetCachePieceBudgetDiv:
		admit = false
		stats.Record(context.Background(), OffsetCacheRefusedLargeCount.M(1))

	case oc.used+cost > oc.budget:
		// competitive eviction: displace the lowest-score pieces only if
		// the candidate beats their combined windowed score by the margin
		type evCand struct {
			key   string
			po    *pieceOffsets
			score int64
		}
		all := make([]evCand, 0, len(oc.pieces))
		for k, p := range oc.pieces {
			all = append(all, evCand{k, p, p.hits.score()})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })

		var freed, evScore int64
		var evict []evCand
		for _, cd := range all {
			if oc.used+cost-freed <= oc.budget {
				break
			}
			evict = append(evict, cd)
			freed += cd.po.bytes
			evScore += cd.score
		}

		if oc.used+cost-freed > oc.budget ||
			score <= evScore+(evScore*offsetCacheEvictPct)/100+offsetCacheAdmitMin {
			admit = false
			stats.Record(context.Background(), OffsetCacheLostCompareCount.M(1))
		} else {
			for _, cd := range evict {
				delete(oc.pieces, cd.key)
				oc.used -= cd.po.bytes
				evicted = append(evicted, cd.po)
			}
			stats.Record(context.Background(),
				OffsetCacheEvictCount.M(int64(len(evict))))
		}
	}

	if !admit {
		oc.cooldown[key] = time.Now().Add(offsetCacheCooldown)
		oc.admin.Unlock()
		stats.Record(context.Background(), OffsetCacheAdminLockHoldMs.M(float64(time.Since(lockStart).Microseconds())/1000))
		return
	}

	oc.pieces[key] = po
	oc.used += cost
	used, npieces := oc.used, len(oc.pieces)
	oc.admin.Unlock()
	stats.Record(context.Background(), OffsetCacheAdminLockHoldMs.M(float64(time.Since(lockStart).Microseconds())/1000))

	// reverse-map maintenance runs outside the admin lock, batched per shard
	for _, ev := range evicted {
		oc.removeRev(ev)
	}
	oc.insertRev(po)

	stats.Record(context.Background(),
		OffsetCacheBytesUsed.M(used),
		OffsetCachePiecesCached.M(int64(npieces)))

	log.Debugw("offset cache: piece index cached", "piece", piece,
		"entries", len(po.mhs), "bytes", cost, "evicted", len(evicted),
		"loadTook", time.Since(start))
}

// insertRev/removeRev maintain the sharded reverse index, taking each shard
// lock once with all of that shard's entries.
func (oc *offsetCache) insertRev(po *pieceOffsets) {
	var byShard [offsetCacheShards][]int32
	for i, m := range po.mhs {
		s := m[len(m)-1] % offsetCacheShards
		byShard[s] = append(byShard[s], int32(i))
	}
	for s := range byShard {
		if len(byShard[s]) == 0 {
			continue
		}
		sh := &oc.shards[s]
		sh.mu.Lock()
		for _, i := range byShard[s] {
			sh.m[po.mhs[i]] = revEntry{po: po, idx: i}
		}
		sh.mu.Unlock()
	}
}

func (oc *offsetCache) removeRev(po *pieceOffsets) {
	var byShard [offsetCacheShards][]int32
	for i, m := range po.mhs {
		s := m[len(m)-1] % offsetCacheShards
		byShard[s] = append(byShard[s], int32(i))
	}
	for s := range byShard {
		if len(byShard[s]) == 0 {
			continue
		}
		sh := &oc.shards[s]
		sh.mu.Lock()
		for _, i := range byShard[s] {
			if e, ok := sh.m[po.mhs[i]]; ok && e.po == po {
				delete(sh.m, po.mhs[i])
			}
		}
		sh.mu.Unlock()
	}
}

// invalidate drops a piece from the cache (e.g. after a read through cached
// offsets failed) and puts it on cooldown.
func (oc *offsetCache) invalidate(piece cid.Cid) {
	key := piece.KeyString()

	oc.admin.Lock()
	po, ok := oc.pieces[key]
	if ok {
		delete(oc.pieces, key)
		oc.used -= po.bytes
	}
	oc.cooldown[key] = time.Now().Add(offsetCacheCooldown)
	used, npieces := oc.used, len(oc.pieces)
	oc.admin.Unlock()

	if ok {
		oc.removeRev(po)
		stats.Record(context.Background(),
			OffsetCacheEvictCount.M(1),
			OffsetCacheBytesUsed.M(used),
			OffsetCachePiecesCached.M(int64(npieces)))
	}
}

// rotateLoop advances the access-counter windows, prunes dead candidates and
// expired cooldowns.
func (oc *offsetCache) rotateLoop() {
	t := time.NewTicker(offsetCacheWindow)
	defer t.Stop()

	for range t.C {
		lockStart := time.Now()
		oc.admin.Lock()

		for k, wc := range oc.candidates {
			wc.rotate()
			if wc.score() == 0 {
				delete(oc.candidates, k)
			}
		}
		for _, po := range oc.pieces {
			po.hits.rotate()
		}
		now := time.Now()
		for k, until := range oc.cooldown {
			if now.After(until) {
				delete(oc.cooldown, k)
			}
		}

		oc.admin.Unlock()
		stats.Record(context.Background(), OffsetCacheAdminLockHoldMs.M(float64(time.Since(lockStart).Microseconds())/1000))
	}
}
