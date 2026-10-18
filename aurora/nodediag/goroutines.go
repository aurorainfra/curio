package nodediag

import (
	"bytes"
	"context"
	"regexp"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultGoroutineBytes = 256 << 10
	maxGoroutineBytes     = 2 << 20
	maxTopFrames          = 40
	maxGoroutineIDs       = 10
)

type GoroutinesRequest struct {
	// Filter keeps goroutines whose stack contains this literal substring
	// (function or file names).
	Filter string
	// State keeps goroutines whose wait state contains this substring, e.g.
	// "chan receive", "select", "semacquire", "IO wait", "running".
	State string
	// MinWaitMinutes keeps goroutines blocked for at least this long.
	MinWaitMinutes int
	// MaxBytes caps the returned stacks.
	MaxBytes int
}

type GoroutineGroup struct {
	// Count of goroutines sharing this stack.
	Count int
	State string
	// MaxWaitMinutes is the longest wait in the group.
	MaxWaitMinutes int
	// IDs of a few goroutines in the group.
	IDs []int64
	// Stack of one goroutine in the group, with argument values stripped.
	Stack string
}

type FrameCount struct {
	Frame string
	Count int
}

type GoroutinesResponse struct {
	Total   int
	Matched int
	ByState map[string]int
	// TopFrames counts matched goroutines by their first non-runtime frame.
	TopFrames []FrameCount
	// Groups of matched goroutines with identical stacks, largest first.
	Groups    []GoroutineGroup
	Truncated bool
}

type goroutine struct {
	id      int64
	state   string
	waitMin int
	stack   string // full text, header included
	sig     string // function lines only, for grouping
	top     string
}

var goroutineHeader = regexp.MustCompile(`^goroutine (\d+)(?: gp=\S+ m=\S+(?: mp=\S+)?)? \[([^\]]*)\]:`)

func parseGoroutines(dump []byte) []goroutine {
	var out []goroutine
	for _, block := range bytes.Split(dump, []byte("\n\n")) {
		block = bytes.TrimSpace(block)
		if len(block) == 0 {
			continue
		}
		lines := strings.Split(string(block), "\n")
		hm := goroutineHeader.FindStringSubmatch(lines[0])
		if hm == nil {
			continue
		}
		g := goroutine{stack: string(block)}
		g.id, _ = strconv.ParseInt(hm[1], 10, 64)

		// "[chan receive, 12 minutes, locked to thread]"
		parts := strings.Split(hm[2], ", ")
		g.state = parts[0]
		for _, p := range parts[1:] {
			if strings.HasSuffix(p, " minutes") || strings.HasSuffix(p, " minute") {
				g.waitMin, _ = strconv.Atoi(strings.Fields(p)[0])
			}
		}

		var sig strings.Builder
		sig.WriteString(g.state)
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, "\t") {
				continue // file:line +0xNN
			}
			fn := l
			if i := strings.LastIndexByte(fn, '('); i > 0 {
				fn = fn[:i]
			}
			fn = strings.TrimPrefix(fn, "created by ")
			if i := strings.Index(fn, " in goroutine "); i > 0 {
				fn = fn[:i]
			}
			sig.WriteString("\n")
			sig.WriteString(fn)
			if g.top == "" && !strings.HasPrefix(fn, "runtime.") && !strings.HasPrefix(fn, "sync.") && !strings.HasPrefix(fn, "internal/") {
				g.top = fn
			}
		}
		g.sig = sig.String()
		if g.top == "" {
			g.top = g.state
		}
		out = append(out, g)
	}
	return out
}

// stripArgs removes argument values ("(0xc000123, 0x1)") from function
// lines so goroutines in the same place print identically.
var argsRe = regexp.MustCompile(`\((?:0x[0-9a-f]+|\.\.\.|\{[^}]*\}|, )*\)$`)

func stripArgs(stack string) string {
	lines := strings.Split(stack, "\n")
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, "\t") {
			lines[i+1] = argsRe.ReplaceAllString(l, "(...)")
		}
	}
	return strings.Join(lines, "\n")
}

func goroutines(_ context.Context, req GoroutinesRequest) (*GoroutinesResponse, error) {
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		return nil, err
	}
	return filterGoroutines(buf.Bytes(), req), nil
}

func filterGoroutines(dump []byte, req GoroutinesRequest) *GoroutinesResponse {
	maxBytes := clampInt(req.MaxBytes, defaultGoroutineBytes, maxGoroutineBytes)
	all := parseGoroutines(dump)

	resp := &GoroutinesResponse{Total: len(all), ByState: map[string]int{}}
	groups := map[string]*GoroutineGroup{}
	tops := map[string]int{}
	var order []string

	for _, g := range all {
		if req.Filter != "" && !strings.Contains(g.stack, req.Filter) {
			continue
		}
		if req.State != "" && !strings.Contains(g.state, req.State) {
			continue
		}
		if g.waitMin < req.MinWaitMinutes {
			continue
		}
		resp.Matched++
		resp.ByState[g.state]++
		tops[g.top]++

		gr, ok := groups[g.sig]
		if !ok {
			gr = &GoroutineGroup{State: g.state, Stack: stripArgs(g.stack)}
			groups[g.sig] = gr
			order = append(order, g.sig)
		}
		gr.Count++
		gr.MaxWaitMinutes = max(gr.MaxWaitMinutes, g.waitMin)
		if len(gr.IDs) < maxGoroutineIDs {
			gr.IDs = append(gr.IDs, g.id)
		}
	}

	for f, c := range tops {
		resp.TopFrames = append(resp.TopFrames, FrameCount{Frame: f, Count: c})
	}
	sort.Slice(resp.TopFrames, func(i, j int) bool {
		if resp.TopFrames[i].Count != resp.TopFrames[j].Count {
			return resp.TopFrames[i].Count > resp.TopFrames[j].Count
		}
		return resp.TopFrames[i].Frame < resp.TopFrames[j].Frame
	})
	if len(resp.TopFrames) > maxTopFrames {
		resp.TopFrames = resp.TopFrames[:maxTopFrames]
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.MaxWaitMinutes > b.MaxWaitMinutes
	})
	size := 0
	for _, sig := range order {
		gr := groups[sig]
		size += len(gr.Stack) + 64
		if size > maxBytes {
			resp.Truncated = true
			break
		}
		resp.Groups = append(resp.Groups, *gr)
	}
	return resp
}
