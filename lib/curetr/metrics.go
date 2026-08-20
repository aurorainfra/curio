package curetr

import (
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
)

var millisecondsDistribution = view.Distribution(
	0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500,
	1000, 2000, 5000, 10000, 30000, 60000,
)

var blockBytesDistribution = view.Distribution(
	1<<10, 4<<10, 16<<10, 64<<10, 128<<10, 256<<10, 512<<10,
	1<<20, 2<<20, 4<<20, 8<<20, 16<<20, 64<<20,
)

var (
	// request / response counters
	RawGetRequestCount  = stats.Int64("curetr/raw_get_request_count", "Counter of raw-block GET requests", stats.UnitDimensionless)
	RawHeadRequestCount = stats.Int64("curetr/raw_head_request_count", "Counter of raw-block HEAD requests", stats.UnitDimensionless)
	Raw200ResponseCount = stats.Int64("curetr/raw_200_response_count", "Counter of raw-block 200 responses", stats.UnitDimensionless)
	Raw400ResponseCount = stats.Int64("curetr/raw_400_response_count", "Counter of raw-block 400 responses (bad cid)", stats.UnitDimensionless)
	Raw404ResponseCount = stats.Int64("curetr/raw_404_response_count", "Counter of raw-block 404 responses", stats.UnitDimensionless)
	Raw500ResponseCount = stats.Int64("curetr/raw_500_response_count", "Counter of raw-block 500 responses", stats.UnitDimensionless)

	// payload detail
	RawBytesServedCount = stats.Int64("curetr/raw_bytes_served_count", "Total block bytes served by the raw fast path", stats.UnitBytes)
	RawBlockBytes       = stats.Int64("curetr/raw_block_bytes", "Distribution of served block sizes", stats.UnitBytes)

	// timing detail: fetch is the blockstore Get (backend: caches, CQL,
	// piece read); write is the response write (client / network
	// backpressure); duration is the whole request.
	RawRequestDurationMs = stats.Float64("curetr/raw_request_duration_ms", "Time to serve a raw-block request end to end", stats.UnitMilliseconds)
	RawFetchDurationMs   = stats.Float64("curetr/raw_fetch_duration_ms", "Time spent fetching the block from the blockstore", stats.UnitMilliseconds)
	RawWriteDurationMs   = stats.Float64("curetr/raw_write_duration_ms", "Time spent writing the block to the client", stats.UnitMilliseconds)

	// load
	RawInflight = stats.Int64("curetr/raw_inflight", "Raw-block requests currently being served", stats.UnitDimensionless)
)

func init() {
	err := view.Register(
		&view.View{Measure: RawGetRequestCount, Aggregation: view.Sum()},
		&view.View{Measure: RawHeadRequestCount, Aggregation: view.Sum()},
		&view.View{Measure: Raw200ResponseCount, Aggregation: view.Sum()},
		&view.View{Measure: Raw400ResponseCount, Aggregation: view.Sum()},
		&view.View{Measure: Raw404ResponseCount, Aggregation: view.Sum()},
		&view.View{Measure: Raw500ResponseCount, Aggregation: view.Sum()},
		&view.View{Measure: RawBytesServedCount, Aggregation: view.Sum()},
		&view.View{Measure: RawBlockBytes, Aggregation: blockBytesDistribution},
		&view.View{Measure: RawRequestDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: RawFetchDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: RawWriteDurationMs, Aggregation: millisecondsDistribution},
		&view.View{Measure: RawInflight, Aggregation: view.LastValue()},
	)
	if err != nil {
		panic(err)
	}
}
