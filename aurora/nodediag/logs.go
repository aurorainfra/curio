package nodediag

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/filecoin-project/curio/deps"
)

const (
	defaultReadBytes = 64 << 10
	maxReadBytes     = 1 << 20

	defaultGrepMatches = 200
	maxGrepMatches     = 2000
	maxGrepContext     = 10
	defaultScanBytes   = 8 << 30
	maxScanBytes       = 1 << 40
	maxPatternLen      = 512

	defaultTailLines = 100
	maxTailLines     = 2000
	defaultTailBytes = 256 << 10

	// lines longer than this are matched on their first maxLineBytes bytes
	maxLineBytes = 1 << 20
	// match/context lines returned to the caller are truncated to these sizes
	maxMatchLineOut   = 2 << 10
	maxContextLineOut = 512

	// past this much after Until the scan stops; log lines from different
	// goroutines are only roughly ordered
	untilSlack = 5 * time.Minute

	maxRotatedFiles = 100
	readerBufSize   = 1 << 20
)

// scanSem bounds concurrent log scans on this node.
var scanSem = make(chan struct{}, 4)

func acquireScan(ctx context.Context) (func(), error) {
	select {
	case scanSem <- struct{}{}:
		return func() { <-scanSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// LogFile describes one log file available on the node.
type LogFile struct {
	// Name identifies the file in log requests.
	Name    string
	Size    int64
	ModTime time.Time
	// Active is set for files this process writes to; others are rotated
	// copies.
	Active bool
	// Gzip files are read through a decompressor: offsets are offsets in the
	// decompressed stream and reaching an offset requires decompressing
	// everything before it.
	Gzip bool
	// Source is golog (GOLOG_FILE), stdout, stderr, config or rotated.
	Source string
}

type LogFilesResponse struct {
	Files []LogFile
	Note  string `json:",omitempty"`
}

type logFiles struct {
	configured []string
}

func newLogFiles(d *deps.Deps) *logFiles {
	lf := &logFiles{}
	if d != nil && d.Cfg != nil {
		lf.configured = append(lf.configured, d.Cfg.Aurora.MCP.LogFiles...)
	}
	return lf
}

type logFileEntry struct {
	LogFile
	path string
}

var rotatedSuffix = regexp.MustCompile(`^([.-][0-9A-Za-z_.-]*)$`)

// entries lists the log files of this node. Active files come first, rotated
// copies after them, newest first.
func (lf *logFiles) entries() ([]logFileEntry, error) {
	type cand struct {
		path   string
		source string
	}
	var cands []cand
	if p := logging.GetConfig().File; p != "" {
		cands = append(cands, cand{p, "golog"})
	}
	if p := os.Getenv("GOLOG_FILE"); p != "" {
		cands = append(cands, cand{p, "golog"})
	}
	for fd, src := range map[int]string{1: "stdout", 2: "stderr"} {
		p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		if err != nil || !filepath.IsAbs(p) {
			continue // pipe:[...], socket:[...] (journald) etc.
		}
		cands = append(cands, cand{p, src})
	}
	for _, p := range lf.configured {
		cands = append(cands, cand{p, "config"})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].source == "golog" && cands[j].source != "golog" })

	seen := map[string]bool{}
	names := map[string]bool{}
	var active, rotated []logFileEntry

	add := func(list *[]logFileEntry, p, source string, isActive bool) {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return
		}
		if seen[real] {
			return
		}
		st, err := os.Stat(real)
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		seen[real] = true
		name := filepath.Base(real)
		for i := 2; names[name]; i++ {
			name = fmt.Sprintf("%s#%d", filepath.Base(real), i)
		}
		names[name] = true
		*list = append(*list, logFileEntry{
			LogFile: LogFile{
				Name:    name,
				Size:    st.Size(),
				ModTime: st.ModTime(),
				Active:  isActive,
				Gzip:    strings.HasSuffix(real, ".gz"),
				Source:  source,
			},
			path: real,
		})
	}

	for _, c := range cands {
		add(&active, filepath.Clean(c.path), c.source, true)
	}

	for _, a := range append([]logFileEntry(nil), active...) {
		matches, err := filepath.Glob(a.path + "*")
		if err != nil {
			continue
		}
		var rs []logFileEntry
		for _, m := range matches {
			suffix := strings.TrimPrefix(m, a.path)
			if suffix == "" || !rotatedSuffix.MatchString(suffix) {
				continue
			}
			if hasUnsupportedCompression(m) {
				continue
			}
			add(&rs, m, "rotated", false)
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].ModTime.After(rs[j].ModTime) })
		rotated = append(rotated, rs...)
	}
	if len(rotated) > maxRotatedFiles {
		rotated = rotated[:maxRotatedFiles]
	}

	return append(active, rotated...), nil
}

func hasUnsupportedCompression(p string) bool {
	for _, ext := range []string{".xz", ".zst", ".bz2", ".lz4", ".zip", ".7z"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

func (lf *logFiles) list() (*LogFilesResponse, error) {
	es, err := lf.entries()
	if err != nil {
		return nil, err
	}
	resp := &LogFilesResponse{Files: make([]LogFile, 0, len(es))}
	for _, e := range es {
		resp.Files = append(resp.Files, e.LogFile)
	}
	if len(es) == 0 {
		resp.Note = "no log file found: logs are not written to a file (GOLOG_FILE unset, stdout/stderr not redirected to a file); set Aurora.MCP.LogFiles in a node layer if logs are written elsewhere"
	}
	return resp, nil
}

// resolve finds a log file by name; an empty name selects the first active
// file.
func (lf *logFiles) resolve(name string) (logFileEntry, error) {
	es, err := lf.entries()
	if err != nil {
		return logFileEntry{}, err
	}
	if len(es) == 0 {
		return logFileEntry{}, badRequest("no log files on this node")
	}
	if name == "" {
		return es[0], nil
	}
	for _, e := range es {
		if e.Name == name {
			return e, nil
		}
	}
	return logFileEntry{}, badRequest(fmt.Sprintf("unknown log file %q; use log/files to list files", name))
}

// openFrom opens the log file positioned at offset start. For gzip files the
// decompressed stream is skipped up to start.
func openFrom(ctx context.Context, e logFileEntry, start int64) (io.Reader, func(), error) {
	f, err := os.Open(e.path)
	if err != nil {
		return nil, nil, err
	}
	if !e.Gzip {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		return f, func() { _ = f.Close() }, nil
	}

	gz, err := gzip.NewReader(bufio.NewReaderSize(f, readerBufSize))
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	closer := func() { _ = gz.Close(); _ = f.Close() }
	for skipped := int64(0); skipped < start; {
		if err := ctx.Err(); err != nil {
			closer()
			return nil, nil, err
		}
		n, err := io.CopyN(io.Discard, gz, min(start-skipped, 64<<20))
		skipped += n
		if err != nil {
			closer()
			if errors.Is(err, io.EOF) {
				return nil, nil, badRequest(fmt.Sprintf("offset %d is past the end of the decompressed file (%d bytes)", start, skipped))
			}
			return nil, nil, err
		}
	}
	return gz, closer, nil
}

// lineReader reads lines while tracking their byte offsets.
type lineReader struct {
	r   *bufio.Reader
	off int64 // offset of the next unread byte
	buf []byte
}

func newLineReader(r io.Reader, off int64) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, readerBufSize), off: off}
}

// next returns the next line without the trailing newline and the offset at
// which it starts. Lines longer than maxLineBytes are truncated. The returned
// slice is only valid until the next call. At the end of input a final line
// without a newline is returned with a nil error; io.EOF follows.
func (lr *lineReader) next() ([]byte, int64, error) {
	start := lr.off
	sl, err := lr.r.ReadSlice('\n')
	lr.off += int64(len(sl))
	switch {
	case err == nil:
		return trimEOL(sl), start, nil
	case errors.Is(err, bufio.ErrBufferFull):
		lr.buf = append(lr.buf[:0], sl[:min(len(sl), maxLineBytes)]...)
		for errors.Is(err, bufio.ErrBufferFull) {
			sl, err = lr.r.ReadSlice('\n')
			lr.off += int64(len(sl))
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, start, err
		}
		return lr.buf, start, nil
	case errors.Is(err, io.EOF):
		if len(sl) == 0 {
			return nil, start, io.EOF
		}
		return trimEOL(sl), start, nil
	default:
		return nil, start, err
	}
}

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte{'\n'})
	return bytes.TrimSuffix(b, []byte{'\r'})
}

// skipPartial positions the reader at the first line starting at or after
// the requested offset. The reader must have been opened at offset-1 (or 0).
func (lr *lineReader) skipPartial(requested int64) error {
	if requested == 0 {
		return nil
	}
	b, err := lr.r.ReadByte()
	if err != nil {
		return err
	}
	lr.off++
	if b == '\n' {
		return nil
	}
	_, _, err = lr.next()
	return err
}

func truncLine(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return fmt.Sprintf("%s…[+%d bytes]", b[:n], len(b)-n)
}

// ---- read ----

type ReadRequest struct {
	File  string
	Start int64
	// End is exclusive; 0 reads MaxBytes from Start.
	End      int64
	MaxBytes int
	// Raw disables snapping the range to whole lines.
	Raw bool
}

type ReadResponse struct {
	File string
	// Size of the file; for gzip files the compressed size.
	Size int64
	// Start and End delimit the returned data.
	Start, End int64
	EOF        bool
	Data       string
}

func (lf *logFiles) read(ctx context.Context, req ReadRequest) (*ReadResponse, error) {
	e, err := lf.resolve(req.File)
	if err != nil {
		return nil, err
	}
	if req.Start < 0 || (req.End != 0 && req.End <= req.Start) {
		return nil, badRequest("invalid range")
	}
	maxBytes := clampInt(req.MaxBytes, defaultReadBytes, maxReadBytes)
	end := req.Start + int64(maxBytes)
	if req.End != 0 && req.End < end {
		end = req.End
	}
	if !e.Gzip && end > e.Size {
		end = e.Size
	}
	if !e.Gzip && req.Start >= e.Size {
		return &ReadResponse{File: e.Name, Size: e.Size, Start: e.Size, End: e.Size, EOF: true}, nil
	}

	release, err := acquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	from := req.Start
	if !req.Raw && from > 0 {
		from-- // read the byte before Start to know whether Start begins a line
	}
	r, closer, err := openFrom(ctx, e, from)
	if err != nil {
		return nil, err
	}
	defer closer()

	buf := make([]byte, end-from)
	n, err := io.ReadFull(r, buf)
	eof := false
	if err != nil {
		if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return nil, err
		}
		eof = true
	}
	buf = buf[:n]
	if !e.Gzip && from+int64(n) >= e.Size {
		eof = true
	}

	start := from
	if !req.Raw {
		if req.Start > 0 && len(buf) > 0 {
			if buf[0] == '\n' {
				buf, start = buf[1:], start+1
			} else if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf, start = buf[i+1:], start+int64(i+1)
			} else {
				// no line starts in the range
				buf, start = buf[:0], start+int64(len(buf))
			}
		}
		if !eof {
			if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
				buf = buf[:i+1]
			}
		}
	}

	return &ReadResponse{
		File:  e.Name,
		Size:  e.Size,
		Start: start,
		End:   start + int64(len(buf)),
		EOF:   eof && start+int64(len(buf)) == from+int64(n),
		Data:  string(buf),
	}, nil
}

// ---- grep ----

type GrepRequest struct {
	File string
	// Pattern is an RE2 regular expression when Regex is set, a literal
	// otherwise. It may be empty when a level, logger or time filter is set.
	Pattern    string
	Regex      bool
	IgnoreCase bool
	// Start and End (exclusive, 0 = end of file) bound the scanned range.
	Start, End   int64
	MaxMatches   int
	Context      int
	MaxScanBytes int64

	// MinLevel keeps lines at or above the level (debug, info, warn, error).
	MinLevel string
	// Loggers keeps lines from these go-log loggers.
	Loggers []string
	// Since and Until filter by the log line timestamp. Lines without a
	// timestamp (continuations, stack traces) inherit the previous line's.
	Since, Until *time.Time
}

type GrepMatch struct {
	Offset int64
	Line   string
	Before []string `json:",omitempty"`
	After  []string `json:",omitempty"`
}

type GrepResponse struct {
	File string
	Size int64
	// Start is the offset the scan started at (snapped to a line start).
	Start int64
	// ScannedUntil is the offset to resume from when the scan stopped early.
	ScannedUntil int64
	LinesScanned int64
	Matches      []GrepMatch
	// Complete is set when the whole requested range was scanned.
	Complete bool
	// StopReason is set when the scan stopped early: max-matches,
	// scan-limit, past-until or timeout.
	StopReason string `json:",omitempty"`
}

type lineMatcher struct {
	re      *regexp.Regexp
	literal []byte
}

func (m *lineMatcher) match(line []byte) bool {
	switch {
	case m.re != nil:
		return m.re.Match(line)
	case m.literal != nil:
		return bytes.Contains(line, m.literal)
	default:
		return true
	}
}

func newLineMatcher(pattern string, isRegex, ignoreCase bool) (*lineMatcher, error) {
	if len(pattern) > maxPatternLen {
		return nil, badRequest(fmt.Sprintf("pattern longer than %d bytes", maxPatternLen))
	}
	if pattern == "" {
		return &lineMatcher{}, nil
	}
	if !isRegex && !ignoreCase {
		return &lineMatcher{literal: []byte(pattern)}, nil
	}
	if !isRegex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, badRequest("invalid pattern: " + err.Error())
	}
	return &lineMatcher{re: re}, nil
}

type pendingAfter struct {
	idx  int
	left int
}

func (lf *logFiles) grep(ctx context.Context, req GrepRequest) (*GrepResponse, error) {
	e, err := lf.resolve(req.File)
	if err != nil {
		return nil, err
	}
	if req.Start < 0 || (req.End != 0 && req.End <= req.Start) {
		return nil, badRequest("invalid range")
	}
	m, err := newLineMatcher(req.Pattern, req.Regex, req.IgnoreCase)
	if err != nil {
		return nil, err
	}
	filter, err := newMetaFilter(req.MinLevel, req.Loggers, req.Since, req.Until)
	if err != nil {
		return nil, err
	}
	if req.Pattern == "" && filter == nil {
		return nil, badRequest("empty pattern requires a level, logger or time filter")
	}
	maxMatches := clampInt(req.MaxMatches, defaultGrepMatches, maxGrepMatches)
	nctx := min(max(req.Context, 0), maxGrepContext)
	scanLimit := req.MaxScanBytes
	if scanLimit <= 0 {
		scanLimit = defaultScanBytes
	}
	scanLimit = min(scanLimit, maxScanBytes)

	resp := &GrepResponse{File: e.Name, Size: e.Size, Start: req.Start, ScannedUntil: req.Start, Matches: []GrepMatch{}}
	if !e.Gzip && req.Start >= e.Size {
		resp.Start, resp.ScannedUntil, resp.Complete = e.Size, e.Size, true
		return resp, nil
	}

	release, err := acquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	from := max(req.Start-1, 0)
	r, closer, err := openFrom(ctx, e, from)
	if err != nil {
		return nil, err
	}
	defer closer()

	lr := newLineReader(r, from)
	if err := lr.skipPartial(req.Start); err != nil {
		if errors.Is(err, io.EOF) {
			resp.ScannedUntil, resp.Complete = lr.off, true
			return resp, nil
		}
		return nil, err
	}
	resp.Start = lr.off
	resp.ScannedUntil = lr.off

	var before [][]byte // ring of context lines, oldest first
	var pending []pendingAfter
	full := false // max matches reached, only collecting trailing context

	for {
		if resp.LinesScanned%4096 == 0 && ctx.Err() != nil {
			resp.StopReason = "timeout"
			return resp, nil
		}
		line, off, err := lr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !full {
					resp.Complete = true
				}
				return resp, nil
			}
			return nil, err
		}
		if req.End != 0 && off >= req.End {
			if !full {
				resp.Complete = true
			}
			return resp, nil
		}

		for i := range pending {
			if pending[i].left > 0 {
				resp.Matches[pending[i].idx].After = append(resp.Matches[pending[i].idx].After, truncLine(line, maxContextLineOut))
				pending[i].left--
			}
		}
		for len(pending) > 0 && pending[0].left == 0 {
			pending = pending[1:]
		}
		if full {
			if len(pending) == 0 {
				return resp, nil
			}
			continue
		}

		resp.LinesScanned++
		resp.ScannedUntil = lr.off

		ok := true
		if filter != nil {
			var stop bool
			ok, stop = filter.check(line)
			if stop {
				resp.StopReason = "past-until"
				resp.ScannedUntil = off
				return resp, nil
			}
		}
		if ok && m.match(line) {
			gm := GrepMatch{Offset: off, Line: truncLine(line, maxMatchLineOut)}
			for _, b := range before {
				gm.Before = append(gm.Before, truncLine(b, maxContextLineOut))
			}
			resp.Matches = append(resp.Matches, gm)
			if nctx > 0 {
				pending = append(pending, pendingAfter{idx: len(resp.Matches) - 1, left: nctx})
			}
			if len(resp.Matches) >= maxMatches {
				resp.StopReason = "max-matches"
				full = true
				if len(pending) == 0 {
					return resp, nil
				}
				continue
			}
		}

		if nctx > 0 {
			if len(before) == nctx {
				copy(before, before[1:])
				before = before[:nctx-1]
			}
			before = append(before, append([]byte(nil), line[:min(len(line), maxContextLineOut+1)]...))
		}

		if lr.off-resp.Start >= scanLimit {
			resp.StopReason = "scan-limit"
			return resp, nil
		}
	}
}

// ---- tail ----

type TailRequest struct {
	File     string
	Lines    int
	MaxBytes int
}

type TailResponse struct {
	File  string
	Size  int64
	Start int64
	Lines int
	Data  string
}

func (lf *logFiles) tail(ctx context.Context, req TailRequest) (*TailResponse, error) {
	e, err := lf.resolve(req.File)
	if err != nil {
		return nil, err
	}
	if e.Gzip {
		return nil, badRequest("tail is not supported on gzip files; use read or grep")
	}
	nlines := clampInt(req.Lines, defaultTailLines, maxTailLines)
	maxBytes := int64(clampInt(req.MaxBytes, defaultTailBytes, maxReadBytes))

	f, err := os.Open(e.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	from := max(size-maxBytes, 0)
	buf := make([]byte, size-from)
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	start := from
	if from > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf, start = buf[i+1:], start+int64(i+1)
		}
	}
	// keep the last nlines lines
	body := bytes.TrimSuffix(buf, []byte{'\n'})
	cut, count := len(body), 0
	for count < nlines {
		i := bytes.LastIndexByte(body[:cut], '\n')
		count++
		if i < 0 {
			cut = 0
			break
		}
		cut = i
	}
	if cut > 0 {
		buf, start = buf[cut+1:], start+int64(cut+1)
	}
	return &TailResponse{File: e.Name, Size: size, Start: start, Lines: count, Data: string(buf)}, nil
}

// ---- find time ----

type FindTimeRequest struct {
	File string
	Time time.Time
}

type FindTimeResponse struct {
	File string
	Size int64
	// Offset of the first line with a timestamp at or after Time; Size when
	// all lines are older.
	Offset   int64
	LineTime *time.Time `json:",omitempty"`
	Line     string     `json:",omitempty"`
}

// findTimeWindow is where bisection stops and a linear scan takes over.
const findTimeWindow = 256 << 10

// findTimeProbe bounds how far one bisection probe reads looking for a line
// with a timestamp.
const findTimeProbe = 4 << 20

func (lf *logFiles) findTime(ctx context.Context, req FindTimeRequest) (*FindTimeResponse, error) {
	e, err := lf.resolve(req.File)
	if err != nil {
		return nil, err
	}
	if e.Gzip {
		return nil, badRequest("findtime is not supported on gzip files; use grep with Since")
	}
	if req.Time.IsZero() {
		return nil, badRequest("time is required")
	}
	release, err := acquireScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	f, err := os.Open(e.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()

	off, ts, line, ok, err := bisectTime(ctx, f, size, req.Time)
	if err != nil {
		return nil, err
	}
	resp := &FindTimeResponse{File: e.Name, Size: size, Offset: size}
	if ok {
		resp.Offset = off
		resp.LineTime = &ts
		resp.Line = truncLine(line, maxContextLineOut)
	}
	return resp, nil
}

// firstTS returns the first line at or after pos that has a timestamp,
// reading at most limit bytes.
func firstTS(f io.ReaderAt, size, pos, limit int64) (int64, time.Time, []byte, bool, error) {
	from := max(pos-1, 0)
	lr := newLineReader(io.NewSectionReader(f, from, size-from), from)
	if err := lr.skipPartial(pos); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, time.Time{}, nil, false, nil
		}
		return 0, time.Time{}, nil, false, err
	}
	for lr.off-pos < limit {
		line, off, err := lr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, time.Time{}, nil, false, nil
			}
			return 0, time.Time{}, nil, false, err
		}
		if ts, ok := parseMeta(line).time(); ok {
			return off, ts, append([]byte(nil), line...), true, nil
		}
	}
	return 0, time.Time{}, nil, false, nil
}

// bisectTime returns the offset of the first line with a timestamp at or
// after t, by bisecting on line timestamps. ok is false when every line is
// older than t.
func bisectTime(ctx context.Context, f *os.File, size int64, t time.Time) (int64, time.Time, []byte, bool, error) {
	lo, hi := int64(0), size
	for hi-lo > findTimeWindow {
		if err := ctx.Err(); err != nil {
			return 0, time.Time{}, nil, false, err
		}
		mid := lo + (hi-lo)/2
		off, ts, _, ok, err := firstTS(f, size, mid, findTimeProbe)
		if err != nil {
			return 0, time.Time{}, nil, false, err
		}
		if !ok || off >= hi || !ts.Before(t) {
			hi = mid
			continue
		}
		lo = off + 1
	}
	return firstTSAtOrAfter(ctx, f, size, lo, t)
}

func firstTSAtOrAfter(ctx context.Context, f *os.File, size, pos int64, t time.Time) (int64, time.Time, []byte, bool, error) {
	from := max(pos-1, 0)
	lr := newLineReader(io.NewSectionReader(f, from, size-from), from)
	if err := lr.skipPartial(pos); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, time.Time{}, nil, false, nil
		}
		return 0, time.Time{}, nil, false, err
	}
	for n := 0; ; n++ {
		if n%4096 == 0 && ctx.Err() != nil {
			return 0, time.Time{}, nil, false, ctx.Err()
		}
		line, off, err := lr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, time.Time{}, nil, false, nil
			}
			return 0, time.Time{}, nil, false, err
		}
		if ts, ok := parseMeta(line).time(); ok && !ts.Before(t) {
			return off, ts, append([]byte(nil), line...), true, nil
		}
	}
}

func clampInt(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}
