package mcp

import (
	"context"

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
	MaxScanBytes int64    `json:"max_scan_bytes,omitempty" jsonschema:"stop after scanning this many bytes, default 2 GiB, max 20 GiB"`
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
		"Search a log file by streaming over a byte range on one machine or all machines in parallel. Supports literal or RE2 patterns, context lines, and go-log level/logger/time filters (lines without their own timestamp, like stack traces, inherit the previous line's). The scan stops at max_matches, max_scan_bytes, 5 minutes past until, or the tool timeout; resume from ScannedUntil. Each match has its byte offset for log_read.",
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
