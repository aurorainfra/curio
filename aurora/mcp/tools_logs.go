package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/nodediag"
)

type logFilesArgs struct {
	Machine string `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
}

type logReadArgs struct {
	Machine  string `json:"machine" jsonschema:"machine ID, name or host:port"`
	File     string `json:"file,omitempty" jsonschema:"log file name from log_files; default the active log file"`
	Start    int64  `json:"start,omitempty" jsonschema:"start byte offset"`
	End      int64  `json:"end,omitempty" jsonschema:"end byte offset (exclusive); default start+max_bytes"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"max bytes returned, default 65536, max 1048576"`
	Raw      bool   `json:"raw,omitempty" jsonschema:"do not snap the range to whole lines"`
}

type logGrepArgs struct {
	Machine      string   `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\" to search every live machine"`
	File         string   `json:"file,omitempty" jsonschema:"log file name from log_files; default the active log file"`
	Pattern      string   `json:"pattern,omitempty" jsonschema:"literal substring, or RE2 regular expression when regex is set (max 512 bytes); may be empty when a level/logger/time filter is set"`
	Regex        bool     `json:"regex,omitempty" jsonschema:"treat pattern as an RE2 regular expression"`
	IgnoreCase   bool     `json:"ignore_case,omitempty"`
	Start        int64    `json:"start,omitempty" jsonschema:"start byte offset (resume from a previous ScannedUntil, or from log_find_time)"`
	End          int64    `json:"end,omitempty" jsonschema:"end byte offset (exclusive); default end of file"`
	MaxMatches   int      `json:"max_matches,omitempty" jsonschema:"stop after this many matches, default 200, max 2000"`
	Context      int      `json:"context,omitempty" jsonschema:"lines of context before and after each match, max 10"`
	MaxScanBytes int64    `json:"max_scan_bytes,omitempty" jsonschema:"stop after scanning this many bytes, default 8 GiB, max 1 TiB (also bounded by the tool timeout)"`
	MinLevel     string   `json:"min_level,omitempty" jsonschema:"only lines at or above this level: debug, info, warn, error"`
	Loggers      []string `json:"loggers,omitempty" jsonschema:"only lines from these go-log loggers, e.g. harmonytask, wdpost"`
	Since        string   `json:"since,omitempty" jsonschema:"only lines at or after this time: RFC3339 or duration ago (2h). Combine with start from log_find_time to skip the rest of a large file"`
	Until        string   `json:"until,omitempty" jsonschema:"only lines at or before this time; the scan stops 5 minutes past it"`
}

type logTailArgs struct {
	Machine  string `json:"machine" jsonschema:"machine ID, name or host:port"`
	File     string `json:"file,omitempty" jsonschema:"log file name from log_files; default the active log file"`
	Lines    int    `json:"lines,omitempty" jsonschema:"number of lines, default 100, max 2000"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"max bytes read from the end, default 262144, max 1048576"`
}

type logFindTimeArgs struct {
	Machine string `json:"machine" jsonschema:"machine ID, name or host:port"`
	File    string `json:"file,omitempty" jsonschema:"log file name from log_files; default the active log file"`
	Time    string `json:"time" jsonschema:"RFC3339 time or duration ago (2h)"`
}

func (s *Server) registerLogTools() {
	s.registerLogQueryTools()

	addTool(s, "log_files",
		"List the log files of a machine (or all machines): the active go-log file (GOLOG_FILE or redirected stdout/stderr) and rotated copies, with sizes. Log files can be hundreds of GB: always work with byte ranges.",
		func(ctx context.Context, in logFilesArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[nodediag.LogFilesResponse](ctx, s, ms, nodediag.OpLogFiles, struct{}{}), nil
		})

	addTool(s, "log_read",
		"Read a byte range of a log file on one machine. The range is snapped to whole lines unless raw is set; the response gives the actual Start/End to continue from.",
		func(ctx context.Context, in logReadArgs) (any, error) {
			m, err := s.selectMachine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			return callNode[nodediag.ReadResponse](ctx, s, m, nodediag.OpLogRead, nodediag.ReadRequest{
				File: in.File, Start: in.Start, End: in.End, MaxBytes: in.MaxBytes, Raw: in.Raw,
			})
		})

	addTool(s, "log_grep",
		"Quick synchronous search of a log file (first matches, then stop) on one machine or all machines in parallel; for scanning or aggregating gigabytes use log_query. Supports literal or RE2 patterns, context lines, and go-log level/logger/time filters (lines without their own timestamp, like stack traces, inherit the previous line's). The scan stops at max_matches, max_scan_bytes, 5 minutes past until, or the tool timeout; resume from ScannedUntil. Each match has its byte offset for log_read.",
		func(ctx context.Context, in logGrepArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			since, err := parseTime(in.Since)
			if err != nil {
				return nil, err
			}
			until, err := parseTime(in.Until)
			if err != nil {
				return nil, err
			}
			req := nodediag.GrepRequest{
				File: in.File, Pattern: in.Pattern, Regex: in.Regex, IgnoreCase: in.IgnoreCase,
				Start: in.Start, End: in.End, MaxMatches: in.MaxMatches, Context: in.Context, MaxScanBytes: in.MaxScanBytes,
				MinLevel: in.MinLevel, Loggers: in.Loggers, Since: since, Until: until,
			}
			if len(ms) > 1 && in.File != "" {
				return nil, xerrors.New(`file cannot be set with machine "all"; file names are per machine`)
			}
			return callNodes[nodediag.GrepResponse](ctx, s, ms, nodediag.OpLogGrep, req), nil
		})

	addTool(s, "log_tail",
		"The last lines of a log file on one machine.",
		func(ctx context.Context, in logTailArgs) (any, error) {
			m, err := s.selectMachine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			return callNode[nodediag.TailResponse](ctx, s, m, nodediag.OpLogTail, nodediag.TailRequest{File: in.File, Lines: in.Lines, MaxBytes: in.MaxBytes})
		})

	addTool(s, "log_find_time",
		"Find the byte offset of the first log line at or after a time, by bisecting the file on line timestamps. Use the offset as start for log_grep / log_read to skip the rest of a huge file.",
		func(ctx context.Context, in logFindTimeArgs) (any, error) {
			m, err := s.selectMachine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			t, err := parseTime(in.Time)
			if err != nil {
				return nil, err
			}
			if t == nil {
				return nil, xerrors.New("time is required")
			}
			return callNode[nodediag.FindTimeResponse](ctx, s, m, nodediag.OpLogFind, nodediag.FindTimeRequest{File: in.File, Time: *t})
		})
}

// ---- log query jobs ----

type logStageArg struct {
	Op         string   `json:"op" jsonschema:"include | exclude (line matches pattern) | field | exclude_field (JSON field value matches pattern) | level (min_level) | logger | exclude_logger (loggers)"`
	Pattern    string   `json:"pattern,omitempty" jsonschema:"literal, or RE2 when regex is set (max 512 bytes)"`
	Regex      bool     `json:"regex,omitempty"`
	IgnoreCase bool     `json:"ignore_case,omitempty"`
	Field      string   `json:"field,omitempty" jsonschema:"JSON field for field/exclude_field, e.g. msg, caller, error, task, sector"`
	MinLevel   string   `json:"min_level,omitempty" jsonschema:"debug, info, warn or error"`
	Loggers    []string `json:"loggers,omitempty"`
}

type logQueryArgs struct {
	Machine      string        `json:"machine,omitempty" jsonschema:"machine ID, name or host:port, or \"all\"; not needed with job"`
	Files        []string      `json:"files,omitempty" jsonschema:"log file names from log_files; default the active file; [\"all\"] = every file incl. rotated/gzip, oldest first"`
	Job          string        `json:"job,omitempty" jsonschema:"filter the matched lines kept by an earlier log_query job (ID from its result) instead of log files: chain cheap refinements over a big first pass"`
	Start        int64         `json:"start,omitempty" jsonschema:"start byte offset (single file only)"`
	End          int64         `json:"end,omitempty" jsonschema:"end byte offset (single file only)"`
	Since        string        `json:"since,omitempty" jsonschema:"lines at/after this time (RFC3339 or duration ago); uncompressed files are narrowed by bisection, so a window inside a huge file is cheap"`
	Until        string        `json:"until,omitempty" jsonschema:"lines at/before this time"`
	Stages       []logStageArg `json:"stages,omitempty" jsonschema:"filter chain; a line must pass every stage, in order"`
	Mode         string        `json:"mode,omitempty" jsonschema:"lines (default): matching lines; count: just the count; group: counts by group_by keys"`
	GroupBy      []string      `json:"group_by,omitempty" jsonschema:"1-4 keys: logger, level, caller, msg, template (msg with numbers/CIDs/addresses/UUIDs/hex normalized: best for 'what is spamming'), file, minute, 10m, hour, day, field:<json key>, regex"`
	GroupRegex   string        `json:"group_regex,omitempty" jsonschema:"for group key regex: RE2 whose first capture group (or whole match) is the key"`
	Top          int           `json:"top,omitempty" jsonschema:"groups returned, default 50, max 1000"`
	MaxLines     int           `json:"max_lines,omitempty" jsonschema:"lines returned in lines mode, default 200, max 1000; all matches are kept on the node for log_job_lines"`
	Context      int           `json:"context,omitempty" jsonschema:"context lines around matches (lines mode, disables parallel chunking), max 10"`
	MaxScanBytes int64         `json:"max_scan_bytes,omitempty" jsonschema:"stop after scanning this many bytes in total (default unlimited, bounded by the node's job runtime limit)"`
	NoSpill      bool          `json:"no_spill,omitempty" jsonschema:"do not keep matched lines on the node (no paging/chaining)"`
	WaitSeconds  int           `json:"wait_seconds,omitempty" jsonschema:"how long to wait for completion before returning progress, default and max ~50s; then poll with log_job"`
}

type logJobArgs struct {
	Jobs        []string `json:"jobs" jsonschema:"job IDs as returned by log_query (machineID:jobID)"`
	WaitSeconds int      `json:"wait_seconds,omitempty" jsonschema:"wait up to this long for running jobs, default 30, max ~50"`
}

type logJobLinesArgs struct {
	Job      string `json:"job" jsonschema:"job ID (machineID:jobID)"`
	From     int64  `json:"from,omitempty" jsonschema:"index of the first matched line to return"`
	Count    int    `json:"count,omitempty" jsonschema:"lines, default 200, max 5000"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default 262144, max 4194304"`
}

type logJobCancelArgs struct {
	Job string `json:"job" jsonschema:"job ID (machineID:jobID)"`
}

type logJobsArgs struct {
	Machine string `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
}

// jobRef is a node job ID qualified by its machine.
func jobRef(m machine, id string) string { return fmt.Sprintf("%d:%s", m.ID, id) }

func (s *Server) resolveJob(ctx context.Context, ref string) (machine, string, error) {
	mid, id, ok := strings.Cut(ref, ":")
	if !ok || id == "" {
		return machine{}, "", xerrors.Errorf("invalid job %q, want machineID:jobID", ref)
	}
	m, err := s.selectMachine(ctx, mid)
	return m, id, err
}

type logJobsOut struct {
	Jobs []NodeResult[nodediag.JobStatus]
	// Merged sums counts and groups over machines once all jobs finished.
	Merged *mergedQuery `json:",omitempty"`
	Hint   string       `json:",omitempty"`
}

type mergedQuery struct {
	Matched int64
	Groups  []nodediag.GroupCount `json:",omitempty"`
}

func (s *Server) waitBudget(seconds, def int) int {
	budget := s.toolTimeout() - 10*time.Second
	w := time.Duration(def) * time.Second
	if seconds > 0 {
		w = time.Duration(seconds) * time.Second
	}
	return int(max(min(w, budget, 50*time.Second), 0).Milliseconds())
}

func finishJobs(out *logJobsOut, top int) {
	allDone := true
	var merged mergedQuery
	groups := map[string]*nodediag.GroupCount{}
	for i := range out.Jobs {
		r := out.Jobs[i].Result
		if r == nil {
			continue
		}
		if r.State == "running" {
			allDone = false
			continue
		}
		if r.Result == nil {
			continue
		}
		merged.Matched += r.Result.Matched
		for _, g := range r.Result.Groups {
			k := strings.Join(g.Key, "\x1f")
			if acc, ok := groups[k]; ok {
				acc.Count += g.Count
				continue
			}
			gc := g
			groups[k] = &gc
		}
	}
	if !allDone {
		out.Hint = "jobs still running: poll with log_job; results stay available on the nodes for an hour"
		return
	}
	if len(out.Jobs) < 2 {
		return
	}
	for _, g := range groups {
		merged.Groups = append(merged.Groups, *g)
	}
	sort.Slice(merged.Groups, func(a, b int) bool { return merged.Groups[a].Count > merged.Groups[b].Count })
	if top <= 0 {
		top = 50
	}
	if len(merged.Groups) > top {
		merged.Groups = merged.Groups[:top]
	}
	out.Merged = &merged
}

func (s *Server) registerLogQueryTools() {
	addTool(s, "log_query",
		"Scan gigabytes of logs on one or all machines as a background job: parallel chunked scan through a chain of filter stages (include/exclude patterns, JSON field matches, level, logger), with a time window (bisected on uncompressed files), returning matching lines, a count, or counts grouped by keys (e.g. template, logger, minute, field:<key>). All matched lines are kept on the node: page them with log_job_lines, or refine them with another log_query using job=<id> without rescanning. Returns when done or after wait_seconds; then poll with log_job.",
		func(ctx context.Context, in logQueryArgs) (any, error) {
			since, err := parseTime(in.Since)
			if err != nil {
				return nil, err
			}
			until, err := parseTime(in.Until)
			if err != nil {
				return nil, err
			}
			req := nodediag.QueryRequest{
				Files: in.Files, Start: in.Start, End: in.End, Since: since, Until: until,
				Mode: in.Mode, MaxLines: in.MaxLines, Context: in.Context, MaxScanBytes: in.MaxScanBytes,
				NoSpill: in.NoSpill, WaitMS: s.waitBudget(in.WaitSeconds, 50),
			}
			for _, st := range in.Stages {
				req.Stages = append(req.Stages, nodediag.QueryStage{Op: st.Op, Pattern: st.Pattern, Regex: st.Regex, IgnoreCase: st.IgnoreCase, Field: st.Field, MinLevel: st.MinLevel, Loggers: st.Loggers})
			}
			if len(in.GroupBy) > 0 {
				req.Mode = "group"
				req.Group = &nodediag.GroupSpec{Keys: in.GroupBy, Regex: in.GroupRegex, Top: in.Top}
			}

			var ms []machine
			if in.Job != "" {
				m, id, err := s.resolveJob(ctx, in.Job)
				if err != nil {
					return nil, err
				}
				req.Job = id
				ms = []machine{m}
			} else {
				ms, err = s.selectMachines(ctx, in.Machine, true)
				if err != nil {
					return nil, err
				}
				if len(ms) > 1 && len(in.Files) > 0 && (len(in.Files) != 1 || in.Files[0] != "all") {
					return nil, xerrors.New(`files must be empty or ["all"] with machine "all"; file names are per machine`)
				}
			}

			out := &logJobsOut{Jobs: callNodes[nodediag.JobStatus](ctx, s, ms, nodediag.OpLogQuery, req)}
			for i := range out.Jobs {
				if r := out.Jobs[i].Result; r != nil {
					r.ID = jobRef(ms[i], r.ID)
				}
			}
			finishJobs(out, in.Top)
			return out, nil
		})

	addTool(s, "log_job",
		"Status and results of log_query jobs: progress while running (bytes/lines scanned, matches so far), the full result when done. For several machines, counts and groups are also merged.",
		func(ctx context.Context, in logJobArgs) (any, error) {
			if len(in.Jobs) == 0 || len(in.Jobs) > 64 {
				return nil, xerrors.New("pass 1-64 job IDs")
			}
			out := &logJobsOut{Jobs: make([]NodeResult[nodediag.JobStatus], len(in.Jobs))}
			wait := s.waitBudget(in.WaitSeconds, 30)
			var wg sync.WaitGroup
			for i, ref := range in.Jobs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					m, id, err := s.resolveJob(ctx, ref)
					if err != nil {
						out.Jobs[i].Error = err.Error()
						return
					}
					out.Jobs[i].MachineID, out.Jobs[i].Machine = m.ID, m.Name
					var st nodediag.JobStatus
					if err := s.nodes.call(ctx, m, nodediag.OpLogJob, nodediag.JobRequest{ID: id, WaitMS: wait}, &st); err != nil {
						out.Jobs[i].Error = err.Error()
						return
					}
					st.ID = ref
					out.Jobs[i].Result = &st
				}()
			}
			wg.Wait()
			finishJobs(out, 0)
			return out, nil
		})

	addTool(s, "log_job_lines",
		"Page through all lines matched by a finished log_query job, with their source file and byte offset (use log_read around an offset for context).",
		func(ctx context.Context, in logJobLinesArgs) (any, error) {
			m, id, err := s.resolveJob(ctx, in.Job)
			if err != nil {
				return nil, err
			}
			r, err := callNode[nodediag.JobLinesResponse](ctx, s, m, nodediag.OpLogJobLines, nodediag.JobLinesRequest{ID: id, From: in.From, Count: in.Count, MaxBytes: in.MaxBytes})
			if err != nil {
				return nil, err
			}
			r.Result.ID = in.Job
			return r, nil
		})

	addTool(s, "log_job_cancel",
		"Cancel a running log_query job; its partial matched lines stay available.",
		func(ctx context.Context, in logJobCancelArgs) (any, error) {
			m, id, err := s.resolveJob(ctx, in.Job)
			if err != nil {
				return nil, err
			}
			return callNode[nodediag.JobStatus](ctx, s, m, nodediag.OpLogJobCancel, nodediag.JobRequest{ID: id})
		})

	addTool(s, "log_jobs",
		"List log_query jobs on a machine (or all), with the scratch space used for kept lines and the scan worker count.",
		func(ctx context.Context, in logJobsArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			res := callNodes[nodediag.JobsResponse](ctx, s, ms, nodediag.OpLogJobs, struct{}{})
			for i := range res {
				if res[i].Result == nil {
					continue
				}
				for _, j := range res[i].Result.Jobs {
					j.ID = jobRef(ms[i], j.ID)
				}
			}
			return res, nil
		})
}
