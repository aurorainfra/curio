package nodediag

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Log query engine: scans log files (or the matched lines of an earlier
// query) through a chain of filter stages, in parallel chunks, and returns
// matching lines, a count, or counts grouped by a key. Matched lines are kept
// on the node ("spilled") so the next query can filter them again without
// rescanning gigabytes of logs.

const (
	defaultQueryLines = 200
	maxQueryLines     = 1000
	maxQueryStages    = 16
	maxGroupKeys      = 4
	defaultGroupTop   = 50
	maxGroupTop       = 1000
	maxDistinctGroups = 20000
	otherGroup        = "(other)"

	maxChunks = 64

	// matched lines kept in memory per chunk / returned are cut to this size;
	// the full lines are in the spill
	maxQueryLineOut = 1 << 10
)

// files are split into chunks of at least this size for parallel scans
var minChunkBytes int64 = 128 << 20

// QueryStage is one filter in the chain. A line must pass every stage.
type QueryStage struct {
	// Op is one of:
	//   include / exclude: the line (does not) match Pattern
	//   field / exclude_field: the JSON field Field (does not) match Pattern
	//   level: the line's level is at least MinLevel
	//   logger / exclude_logger: the line's logger is (not) in Loggers
	Op         string
	Pattern    string   `json:",omitempty"`
	Regex      bool     `json:",omitempty"`
	IgnoreCase bool     `json:",omitempty"`
	Field      string   `json:",omitempty"`
	MinLevel   string   `json:",omitempty"`
	Loggers    []string `json:",omitempty"`
}

// GroupSpec groups matching lines by up to 4 keys. Keys: logger, level,
// caller, msg, template (msg with numbers, CIDs, addresses, UUIDs and hex
// normalized), file, minute, 10m, hour, day, field:<json key>, regex (first
// capture group of Regex, or the whole match).
type GroupSpec struct {
	Keys  []string
	Regex string `json:",omitempty"`
	Top   int    `json:",omitempty"`
}

type QueryRequest struct {
	// Files to scan by name; empty scans the active log file, ["all"] scans
	// all files oldest first. Ignored when Job is set.
	Files []string `json:",omitempty"`
	// Job scans the matched lines of an earlier query on this node instead
	// of log files.
	Job string `json:",omitempty"`
	// Start and End bound the scanned byte range; only with a single file.
	Start, End int64 `json:",omitempty"`
	// Since and Until filter by line time. On uncompressed files the scanned
	// range is narrowed to the time window by bisection first.
	Since, Until *time.Time   `json:",omitempty"`
	Stages       []QueryStage `json:",omitempty"`
	// Mode: lines (default), count or group.
	Mode         string     `json:",omitempty"`
	Group        *GroupSpec `json:",omitempty"`
	MaxLines     int        `json:",omitempty"`
	Context      int        `json:",omitempty"`
	MaxScanBytes int64      `json:",omitempty"`
	// NoSpill disables keeping matched lines on the node.
	NoSpill bool `json:",omitempty"`
	// WaitMS is how long the call waits for the job to finish before
	// returning its progress.
	WaitMS int `json:",omitempty"`
}

type QueryMatch struct {
	File   string
	Offset int64
	Line   string
	Before []string `json:",omitempty"`
	After  []string `json:",omitempty"`
}

type GroupCount struct {
	Key     []string
	Count   int64
	FirstTS *time.Time `json:",omitempty"`
	LastTS  *time.Time `json:",omitempty"`
	// Sample is the first matching line of the group.
	Sample       string
	SampleFile   string
	SampleOffset int64
}

type FileScan struct {
	Name string
	// Start and End of the scanned range (End -1: end of a gzip stream).
	Start, End   int64
	BytesScanned int64
	// Skipped explains why the file was not scanned.
	Skipped string `json:",omitempty"`
}

type QueryResult struct {
	Mode    string
	Matched int64
	Matches []QueryMatch `json:",omitempty"`
	// MatchesTruncated: more lines matched than returned; page through
	// them with log/job/lines.
	MatchesTruncated bool         `json:",omitempty"`
	Groups           []GroupCount `json:",omitempty"`
	DistinctGroups   int          `json:",omitempty"`
	// GroupsCapped: more than 20000 distinct keys; the rest were counted
	// under "(other)".
	GroupsCapped bool `json:",omitempty"`
	// Spilled lines can be paged with log/job/lines and filtered further by
	// a query with Job set.
	Spilled        int64 `json:",omitempty"`
	SpillTruncated bool  `json:",omitempty"`
	Files          []FileScan
}

// ---- compiled query ----

type compiledStage struct {
	op       string
	m        *lineMatcher
	field    string
	minLevel int
	loggers  map[string]bool
}

type compiledQuery struct {
	stages   []compiledStage
	mode     string
	keys     []string
	groupRe  *regexp.Regexp
	top      int
	maxLines int
	context  int
	since    *time.Time
	until    *time.Time
	needMeta bool
}

var validGroupKey = regexp.MustCompile(`^(logger|level|caller|msg|template|file|minute|10m|hour|day|regex|field:[A-Za-z0-9_.-]{1,64})$`)

func compileQuery(req QueryRequest) (*compiledQuery, error) {
	q := &compiledQuery{
		mode:    req.Mode,
		context: min(max(req.Context, 0), maxGrepContext),
		since:   req.Since,
		until:   req.Until,
	}
	if q.mode == "" {
		q.mode = "lines"
	}
	switch q.mode {
	case "lines", "count", "group":
	default:
		return nil, badRequest("mode must be lines, count or group")
	}
	q.maxLines = clampInt(req.MaxLines, defaultQueryLines, maxQueryLines)
	if q.since != nil && q.until != nil && q.until.Before(*q.since) {
		return nil, badRequest("until is before since")
	}
	q.needMeta = q.since != nil || q.until != nil

	if len(req.Stages) > maxQueryStages {
		return nil, badRequest(fmt.Sprintf("at most %d stages", maxQueryStages))
	}
	for i, st := range req.Stages {
		cs := compiledStage{op: st.Op, field: st.Field, minLevel: levelUnknown}
		switch st.Op {
		case "include", "exclude":
			if st.Pattern == "" {
				return nil, badRequest(fmt.Sprintf("stage %d: pattern is required", i))
			}
		case "field", "exclude_field":
			if st.Field == "" || len(st.Field) > 64 {
				return nil, badRequest(fmt.Sprintf("stage %d: field is required", i))
			}
		case "level":
			cs.minLevel = parseLevel(st.MinLevel)
			if cs.minLevel == levelUnknown {
				return nil, badRequest(fmt.Sprintf("stage %d: min_level must be debug, info, warn or error", i))
			}
			q.needMeta = true
		case "logger", "exclude_logger":
			if len(st.Loggers) == 0 {
				return nil, badRequest(fmt.Sprintf("stage %d: loggers is required", i))
			}
			cs.loggers = map[string]bool{}
			for _, l := range st.Loggers {
				cs.loggers[l] = true
			}
			q.needMeta = true
		default:
			return nil, badRequest(fmt.Sprintf("stage %d: unknown op %q", i, st.Op))
		}
		if st.Op == "include" || st.Op == "exclude" || st.Op == "field" || st.Op == "exclude_field" {
			m, err := newLineMatcher(st.Pattern, st.Regex, st.IgnoreCase)
			if err != nil {
				return nil, badRequest(fmt.Sprintf("stage %d: %s", i, err))
			}
			cs.m = m
		}
		q.stages = append(q.stages, cs)
	}

	if q.mode == "group" {
		if req.Group == nil || len(req.Group.Keys) == 0 || len(req.Group.Keys) > maxGroupKeys {
			return nil, badRequest(fmt.Sprintf("group mode needs 1-%d group keys", maxGroupKeys))
		}
		for _, k := range req.Group.Keys {
			if !validGroupKey.MatchString(k) {
				return nil, badRequest(fmt.Sprintf("invalid group key %q", k))
			}
			switch k {
			case "logger", "level", "minute", "10m", "hour", "day":
				q.needMeta = true
			case "regex":
				if req.Group.Regex == "" || len(req.Group.Regex) > maxPatternLen {
					return nil, badRequest("group key regex needs a group regex")
				}
				re, err := regexp.Compile(req.Group.Regex)
				if err != nil {
					return nil, badRequest("invalid group regex: " + err.Error())
				}
				q.groupRe = re
			}
		}
		q.keys = req.Group.Keys
		q.top = clampInt(req.Group.Top, defaultGroupTop, maxGroupTop)
	}
	if q.mode == "group" || q.mode == "count" {
		q.context = 0
	}
	return q, nil
}

// ---- per-line evaluation ----

// metaState tracks line metadata, letting continuation lines inherit the
// metadata of the last line that had a timestamp.
type metaState struct {
	last lineMeta
	have bool
}

func (ms *metaState) get(line []byte) (lineMeta, bool) {
	m := parseMeta(line)
	if m.hasTS {
		ms.last, ms.have = m, true
		return m, true
	}
	return ms.last, ms.have
}

func lineMessage(line []byte) []byte {
	if len(line) > 0 && line[0] == '{' {
		v, _ := jsonField(line, "msg")
		return v
	}
	f := bytes.SplitN(stripANSI(line), []byte{'\t'}, 5)
	if len(f) == 5 {
		return f[4]
	}
	return line
}

func lineCaller(line []byte) []byte {
	if len(line) > 0 && line[0] == '{' {
		v, _ := jsonField(line, "caller")
		return v
	}
	f := bytes.SplitN(stripANSI(line), []byte{'\t'}, 5)
	if len(f) >= 4 {
		return f[3]
	}
	return nil
}

var templateRes = []struct {
	re   *regexp.Regexp
	repl []byte
}{
	{regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), []byte("<uuid>")},
	{regexp.MustCompile(`\b(baf|bag|Qm)[A-Za-z0-9]{20,}`), []byte("<cid>")},
	{regexp.MustCompile(`\b[ft][0-4][a-z0-9]{1,90}\b`), []byte("<addr>")},
	{regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-fA-F]{16,}\b`), []byte("<hex>")},
	{regexp.MustCompile(`\d+(\.\d+)?`), []byte("<n>")},
}

func templateOf(msg []byte) string {
	b := msg
	for _, t := range templateRes {
		b = t.re.ReplaceAll(b, t.repl)
	}
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

func timeBucket(t time.Time, key string) string {
	t = t.UTC()
	switch key {
	case "minute":
		return t.Truncate(time.Minute).Format(time.RFC3339)
	case "10m":
		return t.Truncate(10 * time.Minute).Format(time.RFC3339)
	case "hour":
		return t.Truncate(time.Hour).Format(time.RFC3339)
	default:
		return t.Format("2006-01-02")
	}
}

// eval reports whether the line passes the query's time filter and stages,
// and whether the scan can stop because lines are past Until.
func (q *compiledQuery) eval(line []byte, meta lineMeta, haveMeta bool) (bool, bool) {
	if q.since != nil || q.until != nil {
		if !haveMeta || !meta.hasTS {
			return false, false
		}
		if q.until != nil && meta.ts.After(q.until.Add(untilSlack)) {
			return false, true
		}
		if q.since != nil && meta.ts.Before(*q.since) {
			return false, false
		}
		if q.until != nil && meta.ts.After(*q.until) {
			return false, false
		}
	}
	for i := range q.stages {
		st := &q.stages[i]
		switch st.op {
		case "include":
			if !st.m.match(line) {
				return false, false
			}
		case "exclude":
			if st.m.match(line) {
				return false, false
			}
		case "field", "exclude_field":
			v, ok := jsonField(line, st.field)
			match := ok && st.m.match(v)
			if match != (st.op == "field") {
				return false, false
			}
		case "level":
			if !haveMeta || !meta.hasLvl || meta.level < st.minLevel {
				return false, false
			}
		case "logger":
			if !haveMeta || !st.loggers[meta.logger] {
				return false, false
			}
		case "exclude_logger":
			if haveMeta && st.loggers[meta.logger] {
				return false, false
			}
		}
	}
	return true, false
}

var levelNames = func() map[int]string {
	m := map[int]string{}
	for n, r := range levelRanks {
		m[r] = n
	}
	return m
}()

func (q *compiledQuery) groupKey(line []byte, meta lineMeta, haveMeta bool, file string) string {
	parts := make([]string, len(q.keys))
	for i, k := range q.keys {
		var v string
		switch {
		case k == "logger":
			v = meta.logger
		case k == "level":
			v = levelNames[meta.level]
		case k == "caller":
			v = string(lineCaller(line))
		case k == "msg":
			m := lineMessage(line)
			v = string(m[:min(len(m), 300)])
		case k == "template":
			v = templateOf(lineMessage(line))
		case k == "file":
			v = file
		case k == "minute" || k == "10m" || k == "hour" || k == "day":
			if haveMeta && meta.hasTS {
				v = timeBucket(meta.ts, k)
			}
		case k == "regex":
			sm := q.groupRe.FindSubmatch(line)
			switch {
			case sm == nil:
				v = "(no match)"
			case len(sm) > 1:
				v = string(sm[1])
			default:
				v = string(sm[0])
			}
		case strings.HasPrefix(k, "field:"):
			fv, _ := jsonField(line, strings.TrimPrefix(k, "field:"))
			v = string(fv[:min(len(fv), 300)])
		}
		parts[i] = v
	}
	return strings.Join(parts, "\x1f")
}

// ---- scan units ----

type segment struct {
	fileIdx int
	entry   logFileEntry
	// start/end of the byte range of a log file; end -1 = to EOF
	start, end int64
	// spill is set when scanning an earlier job's matched lines
	spill *spillFile
}

type groupAcc struct {
	count        int64
	first, last  time.Time
	sample       string
	sampleFile   int
	sampleOffset int64
}

type segResult struct {
	matches []QueryMatch
	matched int64
	groups  map[string]*groupAcc
	other   int64
	spill   *spillFile
	bytes   int64
	stop    string
	err     error
}

// runSegment scans one segment.
func (j *queryJob) runSegment(ctx context.Context, seg segment, res *segResult) {
	res.groups = map[string]*groupAcc{}

	var lr *lineReader
	var closer func()
	readRecord := func() ([]byte, int, int64, error) {
		line, off, err := lr.next()
		return line, seg.fileIdx, off, err
	}

	if seg.spill != nil {
		f, err := os.Open(seg.spill.path)
		if err != nil {
			res.err = err
			return
		}
		closer = func() { _ = f.Close() }
		lr = newLineReader(f, 0)
		readRecord = func() ([]byte, int, int64, error) {
			for {
				rec, _, err := lr.next()
				if err != nil {
					return nil, 0, 0, err
				}
				fi, off, line, ok := parseSpillRecord(rec)
				if ok {
					return line, fi, off, nil
				}
			}
		}
	} else {
		from := max(seg.start-1, 0)
		r, c, err := openFrom(ctx, seg.entry, from)
		if err != nil {
			res.err = err
			return
		}
		closer = c
		lr = newLineReader(r, from)
		if err := lr.skipPartial(seg.start); err != nil {
			closer()
			if !errors.Is(err, io.EOF) {
				res.err = err
			}
			return
		}
	}
	defer closer()

	if j.spillOn {
		res.spill = j.reg.newSpill(j)
	}
	if res.spill != nil {
		defer res.spill.close()
	}

	q := j.q
	var ms metaState
	var before [][]byte
	type pending struct{ idx, left int }
	var after []pending
	// bytes are counted up to the end of the last line processed, so a line
	// read past a chunk's end is counted only by the chunk owning it
	startOff, doneOff := lr.off, lr.off
	var lines int64
	flush := func() {
		n := doneOff - startOff
		startOff = doneOff
		res.bytes += n
		j.bytesScanned.Add(n)
	}
	defer flush()

	for {
		if lines%4096 == 0 {
			flush()
			if ctx.Err() != nil {
				res.stop = "canceled"
				return
			}
			if j.scanLimit > 0 && j.bytesScanned.Load() >= j.scanLimit {
				res.stop = "scan-limit"
				return
			}
		}
		line, fi, off, err := readRecord()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				res.err = err
			}
			return
		}
		if seg.spill == nil && seg.end >= 0 && off >= seg.end {
			return
		}
		doneOff = lr.off
		lines++
		j.linesScanned.Add(1)

		if q.context > 0 {
			for i := range after {
				if after[i].left > 0 {
					res.matches[after[i].idx].After = append(res.matches[after[i].idx].After, truncLine(line, maxContextLineOut))
					after[i].left--
				}
			}
			for len(after) > 0 && after[0].left == 0 {
				after = after[1:]
			}
		}

		var meta lineMeta
		var haveMeta bool
		if q.needMeta {
			meta, haveMeta = ms.get(line)
		}
		ok, stop := q.eval(line, meta, haveMeta)
		if stop {
			res.stop = "past-until"
			return
		}
		if ok {
			res.matched++
			j.matched.Add(1)
			if res.spill != nil {
				res.spill.write(fi, off, line)
			}
			switch q.mode {
			case "lines":
				if len(res.matches) < q.maxLines {
					m := QueryMatch{File: j.fileNames[fi], Offset: off, Line: truncLine(line, maxQueryLineOut)}
					for _, b := range before {
						m.Before = append(m.Before, truncLine(b, maxContextLineOut))
					}
					res.matches = append(res.matches, m)
					if q.context > 0 {
						after = append(after, pending{len(res.matches) - 1, q.context})
					}
				}
			case "group":
				key := q.groupKey(line, meta, haveMeta, j.fileNames[fi])
				g, ok := res.groups[key]
				if !ok {
					if len(res.groups) >= maxDistinctGroups {
						res.other++
						break
					}
					g = &groupAcc{sample: truncLine(line, maxQueryLineOut), sampleFile: fi, sampleOffset: off}
					res.groups[key] = g
				}
				g.count++
				if haveMeta && meta.hasTS {
					if g.first.IsZero() || meta.ts.Before(g.first) {
						g.first = meta.ts
					}
					if meta.ts.After(g.last) {
						g.last = meta.ts
					}
				}
			}
		}

		if q.context > 0 {
			if len(before) == q.context {
				copy(before, before[1:])
				before = before[:q.context-1]
			}
			before = append(before, append([]byte(nil), line[:min(len(line), maxContextLineOut+1)]...))
		}
	}
}

// ---- merging ----

func (j *queryJob) merge(results []*segResult) *QueryResult {
	q := j.q
	out := &QueryResult{Mode: q.mode, Files: append([]FileScan(nil), j.fileScans...)}
	for i, r := range results {
		seg := j.segments[i]
		switch {
		case seg.spill != nil:
			out.Files[0].BytesScanned += r.bytes
		case seg.fileIdx < len(j.scanIdx):
			out.Files[j.scanIdx[seg.fileIdx]].BytesScanned += r.bytes
		}
	}
	groups := map[string]*groupAcc{}
	var other int64
	for _, r := range results {
		out.Matched += r.matched
		if q.mode == "lines" {
			for _, m := range r.matches {
				if len(out.Matches) < q.maxLines {
					out.Matches = append(out.Matches, m)
				} else {
					out.MatchesTruncated = true
				}
			}
		}
		other += r.other
		for k, g := range r.groups {
			acc, ok := groups[k]
			if !ok {
				if len(groups) >= maxDistinctGroups {
					other += g.count
					continue
				}
				groups[k] = g
				continue
			}
			acc.count += g.count
			if !g.first.IsZero() && (acc.first.IsZero() || g.first.Before(acc.first)) {
				acc.first = g.first
			}
			if g.last.After(acc.last) {
				acc.last = g.last
			}
		}
		if r.spill != nil {
			out.Spilled += r.spill.records
			out.SpillTruncated = out.SpillTruncated || r.spill.truncated
		}
	}
	if q.mode == "lines" && out.Matched > int64(len(out.Matches)) {
		out.MatchesTruncated = true
	}
	if q.mode == "group" {
		out.DistinctGroups = len(groups)
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			ga, gb := groups[keys[a]], groups[keys[b]]
			if ga.count != gb.count {
				return ga.count > gb.count
			}
			return keys[a] < keys[b]
		})
		if other > 0 {
			out.GroupsCapped = true
		}
		for i, k := range keys {
			if i >= q.top {
				break
			}
			g := groups[k]
			gc := GroupCount{Key: strings.Split(k, "\x1f"), Count: g.count, Sample: g.sample, SampleFile: j.fileNames[g.sampleFile], SampleOffset: g.sampleOffset}
			if !g.first.IsZero() {
				f, l := g.first, g.last
				gc.FirstTS, gc.LastTS = &f, &l
			}
			out.Groups = append(out.Groups, gc)
		}
		if other > 0 {
			out.Groups = append(out.Groups, GroupCount{Key: []string{otherGroup}, Count: other})
		}
	}
	return out
}

// ---- planning ----

// plan resolves the query's sources into scan segments.
func (j *queryJob) plan(ctx context.Context, lf *logFiles, req QueryRequest) error {
	if req.Job != "" {
		parent, err := j.reg.get(req.Job)
		if err != nil {
			return err
		}
		parent.mu.Lock()
		state, spills := parent.state, append([]*spillFile(nil), parent.spills...)
		parent.mu.Unlock()
		if state == jobRunning {
			return badRequest("job " + req.Job + " is still running")
		}
		if len(spills) == 0 {
			return badRequest("job " + req.Job + " kept no matched lines (count mode, no_spill, or no matches)")
		}
		j.fileNames = parent.fileNames
		j.parent = parent
		parent.refs.Add(1)
		for _, sp := range spills {
			j.segments = append(j.segments, segment{spill: sp})
			j.bytesTotal += sp.bytes
		}
		j.fileScans = []FileScan{{Name: "job:" + req.Job, End: j.bytesTotal}}
		return nil
	}

	all, err := lf.entries()
	if err != nil {
		return err
	}
	var files []logFileEntry
	switch {
	case len(req.Files) == 0:
		e, err := lf.resolve("")
		if err != nil {
			return err
		}
		files = []logFileEntry{e}
	case len(req.Files) == 1 && req.Files[0] == "all":
		files = append(files, all...)
		// oldest first, so results come out in time order
		sort.SliceStable(files, func(a, b int) bool { return files[a].ModTime.Before(files[b].ModTime) })
	default:
		for _, n := range req.Files {
			e, err := lf.resolve(n)
			if err != nil {
				return err
			}
			files = append(files, e)
		}
	}
	if (req.Start != 0 || req.End != 0) && len(files) != 1 {
		return badRequest("start/end need exactly one file")
	}

	workers := j.reg.workers()
	for _, e := range files {
		fs := FileScan{Name: e.Name, Start: req.Start, End: req.End}
		if fs.End == 0 {
			fs.End = e.Size
			if e.Gzip {
				fs.End = -1
			}
		}
		if req.Since != nil && e.ModTime.Before(*req.Since) && len(files) > 1 {
			fs.Skipped = "last written before since"
			j.fileScans = append(j.fileScans, fs)
			continue
		}
		if !e.Gzip {
			if skip, err := j.narrowToTime(ctx, e, req, &fs); err != nil {
				return err
			} else if skip != "" {
				fs.Skipped = skip
				j.fileScans = append(j.fileScans, fs)
				continue
			}
		}
		idx := len(j.fileNames)
		j.fileNames = append(j.fileNames, e.Name)
		j.scanIdx = append(j.scanIdx, len(j.fileScans))
		j.fileScans = append(j.fileScans, fs)

		if e.Gzip || j.q.context > 0 {
			j.segments = append(j.segments, segment{fileIdx: idx, entry: e, start: fs.Start, end: fs.End})
			if fs.End > 0 {
				j.bytesTotal += fs.End - fs.Start
			} else {
				j.bytesUnknown = true
			}
			continue
		}
		size := fs.End - fs.Start
		j.bytesTotal += size
		n := int(min(int64(maxChunks), max(1, size/minChunkBytes)))
		n = min(n, workers*2)
		n = max(n, 1)
		chunk := size / int64(n)
		for c := 0; c < n; c++ {
			s := fs.Start + int64(c)*chunk
			end := s + chunk
			if c == n-1 {
				end = fs.End
			}
			j.segments = append(j.segments, segment{fileIdx: idx, entry: e, start: s, end: end})
		}
	}
	return nil
}

// narrowToTime limits an uncompressed file's range to the time window.
func (j *queryJob) narrowToTime(ctx context.Context, e logFileEntry, req QueryRequest, fs *FileScan) (string, error) {
	if req.Since == nil && req.Until == nil {
		return "", nil
	}
	f, err := os.Open(e.path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	size := e.Size

	if req.Until != nil && fs.Start == 0 {
		if _, ts, _, ok, err := firstTS(f, size, 0, findTimeProbe); err != nil {
			return "", err
		} else if ok && ts.After(req.Until.Add(untilSlack)) {
			return "first line is after until", nil
		}
	}
	if req.Since != nil && fs.Start == 0 {
		off, _, _, ok, err := bisectTime(ctx, f, size, req.Since.Add(-untilSlack))
		if err != nil {
			return "", err
		}
		if !ok {
			return "all lines are before since", nil
		}
		fs.Start = off
	}
	if req.Until != nil && fs.End == size {
		off, _, _, ok, err := bisectTime(ctx, f, size, req.Until.Add(untilSlack))
		if err != nil {
			return "", err
		}
		if ok {
			fs.End = off
		}
	}
	if fs.End <= fs.Start {
		return "no lines in the time window", nil
	}
	return "", nil
}

// ---- spill ----

// spillFile holds matched lines as records "<file idx> <offset>\t<line>\n".
type spillFile struct {
	path      string
	f         *os.File
	w         *bufio.Writer
	records   int64
	bytes     int64
	truncated bool
	job       *queryJob
	// index[i] is the byte offset of record i*spillIndexEvery
	index []int64
}

const spillIndexEvery = 1024

func parseSpillRecord(rec []byte) (int, int64, []byte, bool) {
	tab := bytes.IndexByte(rec, '\t')
	if tab < 0 {
		return 0, 0, nil, false
	}
	sp := bytes.IndexByte(rec[:tab], ' ')
	if sp < 0 {
		return 0, 0, nil, false
	}
	fi, err1 := strconv.Atoi(string(rec[:sp]))
	off, err2 := strconv.ParseInt(string(rec[sp+1:tab]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, nil, false
	}
	return fi, off, rec[tab+1:], true
}

var spillBufPool = sync.Pool{New: func() any { return new([]byte) }}

func (s *spillFile) write(fileIdx int, off int64, line []byte) {
	if s.truncated || s.w == nil {
		return
	}
	bp := spillBufPool.Get().(*[]byte)
	b := (*bp)[:0]
	b = strconv.AppendInt(b, int64(fileIdx), 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, off, 10)
	b = append(b, '\t')
	b = append(b, line...)
	b = append(b, '\n')
	if !s.job.reg.reserve(int64(len(b))) {
		s.truncated = true
		*bp = b
		spillBufPool.Put(bp)
		return
	}
	if s.records%spillIndexEvery == 0 {
		s.index = append(s.index, s.bytes)
	}
	if _, err := s.w.Write(b); err != nil {
		s.truncated = true
	}
	s.records++
	s.bytes += int64(len(b))
	*bp = b
	spillBufPool.Put(bp)
}

func (s *spillFile) close() {
	if s.w != nil {
		_ = s.w.Flush()
		_ = s.f.Close()
		s.w = nil
	}
}

// ---- job ----

const (
	jobRunning  = "running"
	jobDone     = "done"
	jobFailed   = "failed"
	jobCanceled = "canceled"
)

type queryJob struct {
	id      string
	reg     *jobRegistry
	q       *compiledQuery
	req     QueryRequest
	created time.Time
	cancel  context.CancelFunc
	spillOn bool

	scanLimit    int64
	segments     []segment
	fileNames    []string
	fileScans    []FileScan
	scanIdx      []int // fileIdx -> index in fileScans
	bytesTotal   int64
	bytesUnknown bool
	parent       *queryJob
	refs         atomic.Int32

	bytesScanned atomic.Int64
	linesScanned atomic.Int64
	matched      atomic.Int64

	mu         sync.Mutex
	state      string
	finished   time.Time
	stopReason string
	err        string
	result     *QueryResult
	spills     []*spillFile
	done       chan struct{}
}

// JobStatus describes a log query job.
type JobStatus struct {
	ID           string
	State        string
	Created      time.Time
	ElapsedMS    int64
	BytesScanned int64
	// BytesTotal is the size of the scanned ranges; -1 when it includes
	// gzip files whose decompressed size is unknown.
	BytesTotal   int64
	LinesScanned int64
	Matched      int64
	StopReason   string       `json:",omitempty"`
	Error        string       `json:",omitempty"`
	Query        QueryRequest `json:",omitempty"`
	Result       *QueryResult `json:",omitempty"`
}

func (j *queryJob) status(withResult bool) *JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := time.Now()
	if !j.finished.IsZero() {
		end = j.finished
	}
	st := &JobStatus{
		ID:           j.id,
		State:        j.state,
		Created:      j.created,
		ElapsedMS:    end.Sub(j.created).Milliseconds(),
		BytesScanned: j.bytesScanned.Load(),
		BytesTotal:   j.bytesTotal,
		LinesScanned: j.linesScanned.Load(),
		Matched:      j.matched.Load(),
		StopReason:   j.stopReason,
		Error:        j.err,
		Query:        j.req,
	}
	st.Query.WaitMS = 0
	if j.bytesUnknown {
		st.BytesTotal = -1
	}
	if withResult {
		st.Result = j.result
	}
	return st
}

func (j *queryJob) run(ctx context.Context) {
	defer close(j.done)
	if j.parent != nil {
		defer j.parent.refs.Add(-1)
	}
	results := make([]*segResult, len(j.segments))
	var wg sync.WaitGroup
	for i, seg := range j.segments {
		results[i] = &segResult{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := j.reg.acquireWorker(ctx); err != nil {
				results[i].stop = "canceled"
				return
			}
			defer j.reg.releaseWorker()
			j.runSegment(ctx, seg, results[i])
		}()
	}
	wg.Wait()

	res := j.merge(results)
	var stop, errText string
	for _, r := range results {
		if r.err != nil && errText == "" {
			errText = r.err.Error()
		}
		if r.stop != "" && r.stop != "past-until" && stop == "" {
			stop = r.stop
		}
	}
	if stop == "canceled" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		stop = "max-runtime"
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	for _, r := range results {
		if r.spill != nil && r.spill.records > 0 {
			j.spills = append(j.spills, r.spill)
		} else if r.spill != nil {
			j.reg.dropSpill(r.spill)
		}
	}
	j.result = res
	j.finished = time.Now()
	j.stopReason = stop
	j.err = errText
	switch {
	case errText != "":
		j.state = jobFailed
	case stop == "canceled":
		j.state = jobCanceled
	default:
		j.state = jobDone
	}
}
