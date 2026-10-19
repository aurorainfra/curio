package nodediag

import (
	"compress/gzip"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testRegistry(t *testing.T, paths ...string) *jobRegistry {
	t.Helper()
	r := &jobRegistry{lf: testLogs(paths...), jobs: map[string]*queryJob{}, dir: t.TempDir()}
	r.wcond = newCond(r)
	return r
}

func runQuery(t *testing.T, r *jobRegistry, req QueryRequest) *JobStatus {
	t.Helper()
	req.WaitMS = maxWaitMS
	st, err := r.start(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, jobDone, st.State, "error: %s, stop: %s", st.Error, st.StopReason)
	require.NotNil(t, st.Result)
	return st
}

func TestLogQueryChunkedParity(t *testing.T) {
	p, _ := writeLog(t, t.TempDir(), 200000)
	r := testRegistry(t, p)
	req := QueryRequest{Mode: "count", Stages: []QueryStage{{Op: "include", Pattern: `"level":"error"`}}}

	whole := runQuery(t, r, req)
	require.Equal(t, int64(20000), whole.Result.Matched)

	old := minChunkBytes
	minChunkBytes = 1 << 20
	defer func() { minChunkBytes = old }()
	chunked := runQuery(t, r, req)
	require.Equal(t, whole.Result.Matched, chunked.Result.Matched)
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, st.Size(), chunked.Result.Files[0].BytesScanned)
	require.Equal(t, st.Size(), chunked.BytesScanned)

	// lines come back in file order across chunks
	lines := runQuery(t, r, QueryRequest{Stages: []QueryStage{{Op: "include", Pattern: `"msg":"line 1`}}, MaxLines: 1000})
	require.True(t, lines.Result.MatchesTruncated)
	require.Len(t, lines.Result.Matches, 1000)
	for i := 1; i < len(lines.Result.Matches); i++ {
		require.Greater(t, lines.Result.Matches[i].Offset, lines.Result.Matches[i-1].Offset)
	}
}

func TestLogQueryGroupAndStages(t *testing.T) {
	p, _ := writeLog(t, t.TempDir(), 20000)
	r := testRegistry(t, p)

	st := runQuery(t, r, QueryRequest{
		Mode:   "group",
		Stages: []QueryStage{{Op: "include", Pattern: `"msg"`}},
		Group:  &GroupSpec{Keys: []string{"logger"}},
	})
	require.Len(t, st.Result.Groups, 2)
	require.Equal(t, []string{"harmonytask"}, st.Result.Groups[0].Key)
	require.Equal(t, int64(18000), st.Result.Groups[0].Count)
	require.Equal(t, []string{"wdpost"}, st.Result.Groups[1].Key)
	require.Equal(t, int64(2000), st.Result.Groups[1].Count)
	require.NotNil(t, st.Result.Groups[1].FirstTS)
	require.Equal(t, t0, st.Result.Groups[1].FirstTS.UTC())

	st = runQuery(t, r, QueryRequest{Mode: "group", Stages: []QueryStage{{Op: "include", Pattern: `"msg"`}}, Group: &GroupSpec{Keys: []string{"template"}}})
	require.Len(t, st.Result.Groups, 1)
	require.Equal(t, []string{"line <n>"}, st.Result.Groups[0].Key)

	st = runQuery(t, r, QueryRequest{Mode: "group", Stages: []QueryStage{{Op: "level", MinLevel: "error"}, {Op: "include", Pattern: `"msg"`}},
		Group: &GroupSpec{Keys: []string{"hour", "regex"}, Regex: `line (\d)`}})
	var total int64
	for _, g := range st.Result.Groups {
		require.Len(t, g.Key, 2)
		total += g.Count
	}
	require.Equal(t, int64(2000), total)

	// field stages and exclusions
	st = runQuery(t, r, QueryRequest{Mode: "count", Stages: []QueryStage{
		{Op: "field", Field: "logger", Pattern: "wdpost"},
		{Op: "exclude_field", Field: "msg", Pattern: `^line \d*5\d$`, Regex: true},
	}})
	require.Equal(t, int64(2000-200), st.Result.Matched)

	st = runQuery(t, r, QueryRequest{Mode: "count", Stages: []QueryStage{{Op: "exclude_logger", Loggers: []string{"harmonytask"}}, {Op: "include", Pattern: `"msg"`}}})
	require.Equal(t, int64(2000), st.Result.Matched)

	_, err := r.start(context.Background(), QueryRequest{Mode: "group", Group: &GroupSpec{Keys: []string{"field:a b"}}})
	require.Error(t, err)
	_, err = r.start(context.Background(), QueryRequest{Stages: []QueryStage{{Op: "rm"}}})
	require.Error(t, err)
}

func TestLogQueryTimeWindow(t *testing.T) {
	p, offs := writeLog(t, t.TempDir(), 40000)
	r := testRegistry(t, p)
	since, until := t0.Add(10000*time.Second), t0.Add(10999*time.Second)
	st := runQuery(t, r, QueryRequest{Mode: "count", Since: &since, Until: &until, Stages: []QueryStage{{Op: "include", Pattern: `"msg"`}}})
	require.Equal(t, int64(1000), st.Result.Matched)
	// the scanned range was narrowed by bisection, with the slack around it
	fs := st.Result.Files[0]
	require.Greater(t, fs.Start, offs[9000])
	require.LessOrEqual(t, fs.Start, offs[10000])
	require.Less(t, fs.End, offs[12000])
	// 1000 lines in the window plus 5 minutes of slack on each side
	require.Less(t, st.BytesScanned, offs[1700])
}

func TestLogQueryChainAndPage(t *testing.T) {
	p, offs := writeLog(t, t.TempDir(), 50000)
	r := testRegistry(t, p)
	old := minChunkBytes
	minChunkBytes = 1 << 20
	defer func() { minChunkBytes = old }()

	first := runQuery(t, r, QueryRequest{Mode: "count", Stages: []QueryStage{{Op: "include", Pattern: `"level":"error"`}}})
	require.Equal(t, int64(5000), first.Result.Matched)
	require.Equal(t, int64(5000), first.Result.Spilled)

	// refine the spill: errors whose line number ends in 00
	second := runQuery(t, r, QueryRequest{Job: first.ID, Stages: []QueryStage{{Op: "include", Pattern: `line \d*00"`, Regex: true}}, MaxLines: 1000})
	require.Equal(t, int64(499), second.Result.Matched) // 100, 200, ... 49900
	require.Equal(t, "curio.log", second.Result.Matches[0].File)
	require.Equal(t, offs[100], second.Result.Matches[0].Offset)
	require.Equal(t, offs[200], second.Result.Matches[1].Offset)

	// page the first job's spill from the middle
	pg, err := r.lines(context.Background(), JobLinesRequest{ID: first.ID, From: 2500, Count: 3})
	require.NoError(t, err)
	require.Equal(t, int64(5000), pg.Total)
	require.Equal(t, int64(2503), pg.Next)
	require.Len(t, pg.Lines, 3)
	require.Equal(t, offs[25000], pg.Lines[0].Offset)
	require.Contains(t, pg.Lines[0].Line, `"msg":"line 25000"`)
	require.Equal(t, offs[25020], pg.Lines[2].Offset)

	pg, err = r.lines(context.Background(), JobLinesRequest{ID: first.ID, From: 4999, Count: 10})
	require.NoError(t, err)
	require.Len(t, pg.Lines, 1)
	require.Equal(t, pg.Total, pg.Next)

	jobs, err := r.list(context.Background(), struct{}{})
	require.NoError(t, err)
	require.Len(t, jobs.Jobs, 2)
	require.Positive(t, jobs.ScratchUsed)

	// no_spill jobs cannot be chained
	ns := runQuery(t, r, QueryRequest{Mode: "count", NoSpill: true, Stages: []QueryStage{{Op: "include", Pattern: "x"}}})
	_, err = r.start(context.Background(), QueryRequest{Job: ns.ID})
	require.Error(t, err)

}

func TestLogQueryAllFiles(t *testing.T) {
	dir := t.TempDir()
	p, _ := writeLog(t, dir, 3000)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	gz, err := os.Create(p + ".1.gz")
	require.NoError(t, err)
	zw := gzip.NewWriter(gz)
	_, err = zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, gz.Close())
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(p+".1.gz", old, old))

	r := testRegistry(t, p)
	st := runQuery(t, r, QueryRequest{Files: []string{"all"}, Mode: "group", Stages: []QueryStage{{Op: "include", Pattern: `"level":"error"`}}, Group: &GroupSpec{Keys: []string{"file"}}})
	require.Equal(t, int64(600), st.Result.Matched)
	require.Len(t, st.Result.Files, 2)
	require.Equal(t, "curio.log.1.gz", st.Result.Files[0].Name, "oldest first")
	require.Equal(t, int64(-1), st.BytesTotal)
	for _, g := range st.Result.Groups {
		require.Equal(t, int64(300), g.Count)
	}

	// since after the rotated file's mtime skips it
	since := time.Now().Add(-time.Minute)
	st = runQuery(t, r, QueryRequest{Files: []string{"all"}, Mode: "count", Since: &since})
	require.Equal(t, "last written before since", st.Result.Files[0].Skipped)
}

func TestLogQueryCancel(t *testing.T) {
	p, _ := writeLog(t, t.TempDir(), 300000)
	r := testRegistry(t, p)
	st, err := r.start(context.Background(), QueryRequest{Mode: "count", Stages: []QueryStage{{Op: "include", Pattern: `(a+)+$`, Regex: true}}})
	require.NoError(t, err)
	cst, err := r.cancelJob(context.Background(), JobRequest{ID: st.ID})
	require.NoError(t, err)
	require.Contains(t, []string{jobCanceled, jobDone}, cst.State)
	_, err = r.lines(context.Background(), JobLinesRequest{ID: "nope"})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "unknown"))
}
