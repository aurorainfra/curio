package curetr

import (
	"sync/atomic"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
)

var bulkInflight atomic.Int64

var rangeBytesDistribution = view.Distribution(
	64<<10, 256<<10, 512<<10, 1<<20, 2<<20, 4<<20, 8<<20, 16<<20, 32<<20,
)

var rangeBlocksDistribution = view.Distribution(
	1, 2, 4, 8, 16, 32, 64, 128, 256,
)

var (
	// request / response counters
	BulkRequestCount     = stats.Int64("curetr/bulk_request_count", "Counter of bulk block requests", stats.UnitDimensionless)
	Bulk400ResponseCount = stats.Int64("curetr/bulk_400_response_count", "Counter of bulk 400 responses (bad request)", stats.UnitDimensionless)
	BulkBlocksRequested  = stats.Int64("curetr/bulk_blocks_requested", "Total blocks requested through bulk requests", stats.UnitDimensionless)

	// per-frame outcomes
	BulkFramesOk       = stats.Int64("curetr/bulk_frames_ok", "Counter of ok bulk frames", stats.UnitDimensionless)
	BulkFramesNotFound = stats.Int64("curetr/bulk_frames_notfound", "Counter of not-found bulk frames", stats.UnitDimensionless)
	BulkFramesError    = stats.Int64("curetr/bulk_frames_error", "Counter of error bulk frames", stats.UnitDimensionless)

	// payload
	BulkBytesServedCount = stats.Int64("curetr/bulk_bytes_served_count", "Total block bytes served by the bulk path", stats.UnitBytes)

	// merged-read efficiency: range bytes / blocks per backing ReadAt. The
	// whole point of the bulk path is avg range size >> one block; if these
	// distributions sit at one block per range, merging is not happening.
	BulkRangeBytes  = stats.Int64("curetr/bulk_range_bytes", "Distribution of merged range read sizes", stats.UnitBytes)
	BulkRangeBlocks = stats.Int64("curetr/bulk_range_blocks", "Distribution of blocks served per merged range read", stats.UnitDimensionless)

	// timing: resolve is the parallel cid->location phase per slice; read
	// is one backing ReadAt of a merged range; duration is the whole request.
	BulkRequestDurationMs = stats.Float64("curetr/bulk_request_duration_ms", "Time to serve a bulk request end to end", stats.UnitMilliseconds)
	BulkResolveDurationMs = stats.Float64("curetr/bulk_resolve_duration_ms", "Time to resolve one request slice to piece locations", stats.UnitMilliseconds)
	BulkReadDurationMs    = stats.Float64("curetr/bulk_read_duration_ms", "Time of one merged range read", stats.UnitMilliseconds)

	// load
	BulkInflight = stats.Int64("curetr/bulk_inflight", "Bulk requests currently being served", stats.UnitDimensionless)
)

func init() {
	err := view.Register(
		&view.View{Measure: BulkRequestCount, Aggregation: view.Sum()},
		&view.View{Measure: Bulk400ResponseCount, Aggregation: view.Sum()},
		&view.View{Measure: BulkBlocksRequested, Aggregation: view.Sum()},
		&view.View{Measure: BulkFramesOk, Aggregation: view.Sum()},
		&view.View{Measure: BulkFramesNotFound, Aggregation: view.Sum()},
		&view.View{Measure: BulkFramesError, Aggregation: view.Sum()},
		&view.View{Measure: BulkBytesServedCount, Aggregation: view.Sum()},
		&view.View{Measure: BulkRangeBytes, Aggregation: rangeBytesDistribution},
		&view.View{Measure: BulkRangeBlocks, Aggregation: rangeBlocksDistribution},
		&view.View{Measure: BulkRequestDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: BulkResolveDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: BulkReadDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: BulkInflight, Aggregation: view.LastValue()},
	)
	if err != nil {
		panic(err)
	}
}
