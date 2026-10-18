package nodediag

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// writeLog writes n go-log JSON lines one second apart; every 10th line is
// an error from logger "wdpost", every 7th is followed by a stack trace line
// without a timestamp.
func writeLog(t *testing.T, dir string, n int) (string, []int64) {
	t.Helper()
	p := filepath.Join(dir, "curio.log")
	f, err := os.Create(p)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	var offs []int64
	var off int64
	for i := 0; i < n; i++ {
		level, logger := "info", "harmonytask"
		if i%10 == 0 {
			level, logger = "error", "wdpost"
		}
		ts := t0.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000Z0700")
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","logger":"%s","caller":"x/y.go:1","msg":"line %d"}`+"\n", level, ts, logger, i)
		offs = append(offs, off)
		_, _ = w.WriteString(line)
		off += int64(len(line))
		if i%7 == 0 {
			st := fmt.Sprintf("\tgoroutine trace for %d\n", i)
			_, _ = w.WriteString(st)
			off += int64(len(st))
		}
	}
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())
	return p, offs
}

func testLogs(paths ...string) *logFiles {
	return &logFiles{configured: paths}
}

func TestLogReadSnapping(t *testing.T) {
	p, offs := writeLog(t, t.TempDir(), 100)
	lf := testLogs(p)
	ctx := context.Background()

	// start in the middle of line 5: snapped forward to line 6
	r, err := lf.read(ctx, ReadRequest{File: "curio.log", Start: offs[5] + 3, MaxBytes: 1000})
	require.NoError(t, err)
	require.Equal(t, offs[6], r.Start)
	require.True(t, strings.HasPrefix(r.Data, `{"level":"info"`))
	require.True(t, strings.HasSuffix(r.Data, "\n"))
	require.Equal(t, r.Start+int64(len(r.Data)), r.End)
	require.False(t, r.EOF)

	// start exactly at a line start is kept
	r, err = lf.read(ctx, ReadRequest{File: "curio.log", Start: offs[6], MaxBytes: 1000})
	require.NoError(t, err)
	require.Equal(t, offs[6], r.Start)

	// raw read returns exactly the range
	r, err = lf.read(ctx, ReadRequest{File: "curio.log", Start: 10, End: 20, Raw: true})
	require.NoError(t, err)
	require.Equal(t, int64(10), r.Start)
	require.Len(t, r.Data, 10)

	// read to the end
	st, err := os.Stat(p)
	require.NoError(t, err)
	r, err = lf.read(ctx, ReadRequest{File: "curio.log", Start: offs[99], MaxBytes: 1 << 20})
	require.NoError(t, err)
	require.True(t, r.EOF)
	require.Equal(t, st.Size(), r.End)

	_, err = lf.read(ctx, ReadRequest{File: "../etc/passwd"})
	require.Error(t, err)
}

func TestLogGrep(t *testing.T) {
	p, offs := writeLog(t, t.TempDir(), 1000)
	lf := testLogs(p)
	ctx := context.Background()

	// literal
	r, err := lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `"msg":"line 42"`})
	require.NoError(t, err)
	require.True(t, r.Complete)
	require.Len(t, r.Matches, 1)
	require.Equal(t, offs[42], r.Matches[0].Offset)

	// regex, max matches and resume
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `line \d+5"`, Regex: true, MaxMatches: 30})
	require.NoError(t, err)
	require.Len(t, r.Matches, 30)
	require.Equal(t, "max-matches", r.StopReason)
	require.False(t, r.Complete)
	r2, err := lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `line \d+5"`, Regex: true, Start: r.ScannedUntil})
	require.NoError(t, err)
	require.True(t, r2.Complete)
	require.Len(t, r2.Matches, 100-30-1) // 5, 15, ... 995 = 100 lines; "line 5" has no leading digit
	require.Greater(t, r2.Matches[0].Offset, r.Matches[29].Offset)

	// context lines
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `"msg":"line 500"`, Context: 2})
	require.NoError(t, err)
	require.Len(t, r.Matches, 1)
	require.Len(t, r.Matches[0].Before, 2)
	require.Len(t, r.Matches[0].After, 2)
	require.Contains(t, r.Matches[0].Before[1], "line 499")

	// byte range
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `"msg"`, Start: offs[100], End: offs[110]})
	require.NoError(t, err)
	require.Len(t, r.Matches, 10)
	require.True(t, r.Complete)

	// level + logger filters; stack trace lines after error lines (multiples
	// of 70) inherit their metadata
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", MinLevel: "error", Loggers: []string{"wdpost"}, MaxMatches: 2000})
	require.NoError(t, err)
	require.Len(t, r.Matches, 100+15)

	// stack trace lines inherit the previous line's metadata
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: "goroutine trace", MinLevel: "error", MaxMatches: 2000})
	require.NoError(t, err)
	for _, m := range r.Matches {
		var n int
		_, err := fmt.Sscanf(strings.TrimSpace(m.Line), "goroutine trace for %d", &n)
		require.NoError(t, err)
		require.Zero(t, n%10, "trace of line %d is not after an error line", n)
	}
	require.Len(t, r.Matches, 15) // multiples of 70 below 1000

	// time window, stops past until
	since, until := t0.Add(100*time.Second), t0.Add(109*time.Second)
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: `"msg"`, Since: &since, Until: &until})
	require.NoError(t, err)
	require.Len(t, r.Matches, 10)
	require.Equal(t, "past-until", r.StopReason)

	// scan limit
	r, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: "nomatch", MaxScanBytes: 5000})
	require.NoError(t, err)
	require.Equal(t, "scan-limit", r.StopReason)
	require.GreaterOrEqual(t, r.ScannedUntil, int64(5000))

	// bad input
	_, err = lf.grep(ctx, GrepRequest{File: "curio.log"})
	require.Error(t, err)
	_, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: "(", Regex: true})
	require.Error(t, err)
	_, err = lf.grep(ctx, GrepRequest{File: "curio.log", Pattern: strings.Repeat("a", 600)})
	require.Error(t, err)
}

func TestLogGrepGzipAndRotated(t *testing.T) {
	dir := t.TempDir()
	p, offs := writeLog(t, dir, 200)
	data, err := os.ReadFile(p)
	require.NoError(t, err)

	gz, err := os.Create(p + ".1.gz")
	require.NoError(t, err)
	zw := gzip.NewWriter(gz)
	_, err = zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(p+".2.zst", []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.log"), []byte("x"), 0o644))

	lf := testLogs(p)
	files, err := lf.list()
	require.NoError(t, err)
	var names []string
	for _, f := range files.Files {
		names = append(names, f.Name)
	}
	require.Equal(t, []string{"curio.log", "curio.log.1.gz"}, names)
	require.True(t, files.Files[1].Gzip)

	r, err := lf.grep(context.Background(), GrepRequest{File: "curio.log.1.gz", Pattern: `"msg":"line 150"`, Start: offs[100]})
	require.NoError(t, err)
	require.Len(t, r.Matches, 1)
	require.Equal(t, offs[150], r.Matches[0].Offset)

	rr, err := lf.read(context.Background(), ReadRequest{File: "curio.log.1.gz", Start: offs[150], End: offs[151]})
	require.NoError(t, err)
	require.Contains(t, rr.Data, `"msg":"line 150"`)

	_, err = lf.tail(context.Background(), TailRequest{File: "curio.log.1.gz"})
	require.Error(t, err)
}

func TestLogTail(t *testing.T) {
	p, offs := writeLog(t, t.TempDir(), 300)
	lf := testLogs(p)
	r, err := lf.tail(context.Background(), TailRequest{File: "curio.log", Lines: 3})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(r.Data, "\n"), "\n")
	require.Len(t, lines, 3)
	require.Contains(t, lines[2], "line 299")
	require.Equal(t, offs[297], r.Start)
}

func TestLogFindTime(t *testing.T) {
	// big enough for several bisection rounds
	p, offs := writeLog(t, t.TempDir(), 40000)
	lf := testLogs(p)
	ctx := context.Background()

	for _, i := range []int{0, 1, 777, 12345, 39999} {
		r, err := lf.findTime(ctx, FindTimeRequest{File: "curio.log", Time: t0.Add(time.Duration(i) * time.Second)})
		require.NoError(t, err)
		require.Equal(t, offs[i], r.Offset, "line %d", i)
		require.Contains(t, r.Line, fmt.Sprintf(`"msg":"line %d"`, i))
	}

	// between lines: the next line
	r, err := lf.findTime(ctx, FindTimeRequest{File: "curio.log", Time: t0.Add(1500*time.Second + 300*time.Millisecond)})
	require.NoError(t, err)
	require.Equal(t, offs[1501], r.Offset)

	// before the first line
	r, err = lf.findTime(ctx, FindTimeRequest{File: "curio.log", Time: t0.Add(-time.Hour)})
	require.NoError(t, err)
	require.Equal(t, int64(0), r.Offset)

	// after the last line
	r, err = lf.findTime(ctx, FindTimeRequest{File: "curio.log", Time: t0.Add(100 * time.Hour)})
	require.NoError(t, err)
	require.Equal(t, r.Size, r.Offset)
	require.Nil(t, r.LineTime)
}

func TestParseMeta(t *testing.T) {
	m := parseMeta([]byte(`{"level":"warn","ts":"2026-10-18T12:00:01.500+0200","logger":"paths","caller":"a.go:1","msg":"x"}`))
	require.True(t, m.hasTS)
	require.Equal(t, time.Date(2026, 10, 18, 10, 0, 1, 500e6, time.UTC), m.ts.UTC())
	require.Equal(t, levelRanks["warn"], m.level)
	require.Equal(t, "paths", m.logger)

	m = parseMeta([]byte("2026-10-18T12:00:01.500Z\tERROR\tharmonytask\tharmonytask/x.go:1\tfailed"))
	require.True(t, m.hasTS)
	require.Equal(t, levelRanks["error"], m.level)
	require.Equal(t, "harmonytask", m.logger)

	m = parseMeta([]byte("\x1b[90m2026-10-18T12:00:01.500Z\x1b[0m\t\x1b[31mERROR\x1b[0m\tharmonytask\tx.go:1\tfailed"))
	require.True(t, m.hasTS)
	require.Equal(t, levelRanks["error"], m.level)

	m = parseMeta([]byte("\tat some.frame(file.go:12)"))
	require.False(t, m.hasTS)
}
