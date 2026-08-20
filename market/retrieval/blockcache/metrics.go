package blockcache

import (
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

// ClassKey tags block cache metrics with the size class ("32k", "128k", "2m").
var ClassKey, _ = tag.NewKey("class")

var (
	Hit         = stats.Int64("blockcache/hit_count", "Counter of block cache hits", stats.UnitDimensionless)
	Admit       = stats.Int64("blockcache/admit_count", "Counter of blocks admitted to the resident set", stats.UnitDimensionless)
	TrackedOnly = stats.Int64("blockcache/tracked_only_count", "Counter of accesses recorded in the ghost set without admission", stats.UnitDimensionless)
	Evict       = stats.Int64("blockcache/evict_count", "Counter of resident blocks evicted (back to the ghost set)", stats.UnitDimensionless)
	RejectLarge = stats.Int64("blockcache/reject_large_count", "Counter of blocks larger than the largest size class", stats.UnitDimensionless)

	ResidentBytes  = stats.Int64("blockcache/resident_bytes", "Bytes held by the resident set", stats.UnitBytes)
	ResidentBlocks = stats.Int64("blockcache/resident_blocks", "Blocks held by the resident set", stats.UnitDimensionless)
	GhostEntries   = stats.Int64("blockcache/ghost_entries", "Entries in the ghost (tracking) set", stats.UnitDimensionless)
)

func init() {
	classTag := []tag.Key{ClassKey}
	err := view.Register(
		&view.View{Measure: Hit, Aggregation: view.Sum(), TagKeys: classTag},
		&view.View{Measure: Admit, Aggregation: view.Sum(), TagKeys: classTag},
		&view.View{Measure: TrackedOnly, Aggregation: view.Sum(), TagKeys: classTag},
		&view.View{Measure: Evict, Aggregation: view.Sum(), TagKeys: classTag},
		&view.View{Measure: RejectLarge, Aggregation: view.Sum()},
		&view.View{Measure: ResidentBytes, Aggregation: view.LastValue(), TagKeys: classTag},
		&view.View{Measure: ResidentBlocks, Aggregation: view.LastValue(), TagKeys: classTag},
		&view.View{Measure: GhostEntries, Aggregation: view.LastValue(), TagKeys: classTag},
	)
	if err != nil {
		panic(err)
	}
}
