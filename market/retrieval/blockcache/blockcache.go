// Package blockcache is the retrieval block cache: a size-partitioned,
// byte-budgeted block cache with large ghost sets and frequency-gated
// admission.
//
// It replaces a flat 4096-entry ARC whose production hit rate was near zero:
// retrieval traffic is dominated by one-shot reads (sequential streams,
// random benchmarks) which churned every slot without ever re-hitting.
// Design:
//
//   - Blocks are partitioned by size class (default <32KiB, <128KiB, <2MiB;
//     larger blocks are never cached), so many small hot blocks cannot be
//     evicted by a handful of large one-shot blocks and vice versa. Each
//     class has its own resident byte budget (default 1GiB).
//   - Each class keeps a ghost set — keys plus access counters, no data —
//     with its own memory budget (default 128MiB ≈ ~1M entries), giving
//     frequency/recency memory far longer than the resident set.
//   - Admission is frequency-gated: a block is cached only after it has
//     been seen AdmitAfter times in the ghost set (default 1 prior access,
//     i.e. cached on its second access). One-shot reads only ever touch the
//     ghost set and cannot pollute the resident set.
//   - Eviction is LRU within a class; evicted keys return to the ghost set
//     with their frequency, so a still-hot evictee is readmitted on its
//     next access.
package blockcache

import (
	"container/list"
	"context"
	"strconv"
	"sync"

	blocks "github.com/ipfs/go-block-format"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/filecoin-project/lotus/blockstore"
)

// ghostEntryBytes approximates the memory cost of one ghost entry (multihash
// key, map bucket, list element, counter).
const ghostEntryBytes = 140

type Config struct {
	// SizeClasses are the partition upper bounds in bytes, ascending.
	// Blocks larger than the last class are not cached.
	SizeClasses []int
	// PartitionBytes is the resident byte budget of each size class.
	PartitionBytes int64
	// GhostBytes is the tracking (ghost) memory budget of each size class.
	GhostBytes int64
	// AdmitAfter is the number of tracked prior accesses required before a
	// block is admitted to the resident set. 0 admits on first access.
	AdmitAfter int
}

func DefaultConfig() Config {
	return Config{
		SizeClasses:    []int{32 << 10, 128 << 10, 2 << 20},
		PartitionBytes: 1 << 30,
		GhostBytes:     128 << 20,
		AdmitAfter:     1,
	}
}

type entry struct {
	key  string
	blk  blocks.Block
	size int64
	freq uint32
}

type ghostEntry struct {
	key  string
	freq uint32
}

type partition struct {
	tag []tag.Mutator // class tag for metrics

	mu    sync.Mutex
	limit int64
	used  int64

	resident map[string]*list.Element // -> *entry
	lru      *list.List               // front = most recent

	ghostLimit int
	ghost      map[string]*list.Element // -> *ghostEntry
	glru       *list.List
}

// Cache implements the blockstore cache interface used by
// lotus/blockstore.ReadCachedBlockstore (via retrieval.BlockstoreCacheWrap).
type Cache struct {
	classes    []int
	parts      []*partition
	admitAfter uint32
}

func New(cfg Config) *Cache {
	def := DefaultConfig()
	if len(cfg.SizeClasses) == 0 {
		cfg.SizeClasses = def.SizeClasses
	}
	if cfg.PartitionBytes <= 0 {
		cfg.PartitionBytes = def.PartitionBytes
	}
	if cfg.GhostBytes <= 0 {
		cfg.GhostBytes = def.GhostBytes
	}
	if cfg.AdmitAfter < 0 {
		cfg.AdmitAfter = def.AdmitAfter
	}

	c := &Cache{
		classes:    cfg.SizeClasses,
		admitAfter: uint32(cfg.AdmitAfter),
	}
	for _, class := range cfg.SizeClasses {
		c.parts = append(c.parts, &partition{
			tag:        []tag.Mutator{tag.Upsert(ClassKey, classLabel(class))},
			limit:      cfg.PartitionBytes,
			resident:   map[string]*list.Element{},
			lru:        list.New(),
			ghostLimit: int(cfg.GhostBytes / ghostEntryBytes),
			ghost:      map[string]*list.Element{},
			glru:       list.New(),
		})
	}
	return c
}

// classLabel renders a size-class bound as a metric tag value, e.g. "32k",
// "128k", "2m".
func classLabel(bound int) string {
	if bound >= 1<<20 {
		return strconv.Itoa(bound>>20) + "m"
	}
	return strconv.Itoa(bound>>10) + "k"
}

// Get returns a cached block. Hits refresh recency and bump frequency.
func (c *Cache) Get(mh blockstore.MhString) (blocks.Block, bool) {
	key := string(mh)
	for _, p := range c.parts {
		p.mu.Lock()
		if el, ok := p.resident[key]; ok {
			e := el.Value.(*entry)
			e.freq++
			p.lru.MoveToFront(el)
			p.mu.Unlock()
			stats.RecordWithTags(context.Background(), p.tag, Hit.M(1)) //nolint:errcheck
			return e.blk, true
		}
		p.mu.Unlock()
	}
	return nil, false
}

func (c *Cache) Contains(mh blockstore.MhString) bool {
	key := string(mh)
	for _, p := range c.parts {
		p.mu.Lock()
		_, ok := p.resident[key]
		p.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

// Remove drops a block from both the resident and ghost sets (explicit
// deletion, e.g. denylisted content).
func (c *Cache) Remove(mh blockstore.MhString) {
	key := string(mh)
	for _, p := range c.parts {
		p.mu.Lock()
		if el, ok := p.resident[key]; ok {
			e := el.Value.(*entry)
			p.lru.Remove(el)
			delete(p.resident, key)
			p.used -= e.size
		}
		if el, ok := p.ghost[key]; ok {
			p.glru.Remove(el)
			delete(p.ghost, key)
		}
		p.mu.Unlock()
	}
}

// Add records an access to a block and admits it to the resident set once
// its ghost frequency crosses the admission threshold.
func (c *Cache) Add(mh blockstore.MhString, blk blocks.Block) {
	size := int64(len(blk.RawData()))

	pi := -1
	for i, bound := range c.classes {
		if size <= int64(bound) {
			pi = i
			break
		}
	}
	if pi == -1 {
		stats.Record(context.Background(), RejectLarge.M(1))
		return
	}
	p := c.parts[pi]
	if size > p.limit {
		// only possible with a partition budget smaller than its size-class
		// bound; never admit a block that exceeds the whole budget
		stats.Record(context.Background(), RejectLarge.M(1))
		return
	}
	key := string(mh)

	var admitted, tracked bool
	var evicted int64

	p.mu.Lock()

	if el, ok := p.resident[key]; ok {
		// already cached (raced with another request)
		el.Value.(*entry).freq++
		p.lru.MoveToFront(el)
		p.mu.Unlock()
		return
	}

	// ghost bookkeeping
	var prior uint32
	if el, ok := p.ghost[key]; ok {
		ge := el.Value.(*ghostEntry)
		prior = ge.freq
		ge.freq++
		p.glru.MoveToFront(el)
	} else {
		p.ghost[key] = p.glru.PushFront(&ghostEntry{key: key, freq: 1})
		for len(p.ghost) > p.ghostLimit {
			tail := p.glru.Back()
			p.glru.Remove(tail)
			delete(p.ghost, tail.Value.(*ghostEntry).key)
		}
	}

	if prior < c.admitAfter {
		tracked = true
	} else {
		// admit: move from ghost to resident, evicting LRU entries back
		// into the ghost set until the block fits the byte budget
		if el, ok := p.ghost[key]; ok {
			p.glru.Remove(el)
			delete(p.ghost, key)
		}

		for p.used+size > p.limit && p.lru.Len() > 0 {
			tail := p.lru.Back()
			ev := tail.Value.(*entry)
			p.lru.Remove(tail)
			delete(p.resident, ev.key)
			p.used -= ev.size
			evicted++

			// evictees keep their frequency in the ghost set, so still-hot
			// blocks are readmitted on their next access
			p.ghost[ev.key] = p.glru.PushFront(&ghostEntry{key: ev.key, freq: ev.freq})
			for len(p.ghost) > p.ghostLimit {
				gt := p.glru.Back()
				p.glru.Remove(gt)
				delete(p.ghost, gt.Value.(*ghostEntry).key)
			}
		}

		p.resident[key] = p.lru.PushFront(&entry{key: key, blk: blk, size: size, freq: prior + 1})
		p.used += size
		admitted = true
	}

	used := p.used
	nres := len(p.resident)
	nghost := len(p.ghost)
	p.mu.Unlock()

	switch {
	case admitted:
		stats.RecordWithTags(context.Background(), p.tag, //nolint:errcheck
			Admit.M(1), Evict.M(evicted),
			ResidentBytes.M(used), ResidentBlocks.M(int64(nres)), GhostEntries.M(int64(nghost)))
	case tracked:
		stats.RecordWithTags(context.Background(), p.tag, TrackedOnly.M(1)) //nolint:errcheck
	}
}
