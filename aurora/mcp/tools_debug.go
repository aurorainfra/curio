package mcp

import (
	"context"

	"github.com/filecoin-project/curio/aurora/nodediag"
)

type goroutinesArgs struct {
	Machine        string `json:"machine" jsonschema:"machine ID, name or host:port"`
	Filter         string `json:"filter,omitempty" jsonschema:"keep goroutines whose stack contains this literal, e.g. a package or function name (harmonytask, tasks/window)"`
	State          string `json:"state,omitempty" jsonschema:"keep goroutines whose wait state contains this, e.g. chan receive, select, semacquire, IO wait, sync.Mutex.Lock"`
	MinWaitMinutes int    `json:"min_wait_minutes,omitempty" jsonschema:"keep goroutines blocked at least this many minutes"`
	MaxBytes       int    `json:"max_bytes,omitempty" jsonschema:"cap on returned stacks, default 262144, max 2097152"`
}

func (s *Server) registerDebugTools() {
	addTool(s, "goroutines",
		"Live goroutine dump of a machine taken from the Go runtime profiler (pprof goroutine profile, debug=2; not from logs), filtered on the node and grouped: goroutines with identical stacks are merged (with count, longest wait and sample IDs), largest groups first, plus counts by wait state and by top non-runtime frame. Use to find what a stuck task is blocked on.",
		func(ctx context.Context, in goroutinesArgs) (any, error) {
			m, err := s.selectMachine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			return callNode[nodediag.GoroutinesResponse](ctx, s, m, nodediag.OpGoroutines, nodediag.GoroutinesRequest{
				Filter: in.Filter, State: in.State, MinWaitMinutes: in.MinWaitMinutes, MaxBytes: in.MaxBytes,
			})
		})
}
