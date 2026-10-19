package nodediag

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/filecoin-project/curio/deps"
)

const (
	maxRunningJobs   = 8
	maxRetainedJobs  = 64
	jobRetention     = time.Hour
	maxWaitMS        = 55_000
	defaultScratch   = int64(4 << 30)
	defaultJobMaxRun = time.Hour

	defaultPageLines = 200
	maxPageLines     = 5000
	defaultPageBytes = 256 << 10
	maxPageBytes     = 4 << 20
	maxPageLineOut   = 8 << 10
)

type jobRegistry struct {
	d   *deps.Deps
	lf  *logFiles
	dir string

	mu   sync.Mutex
	jobs map[string]*queryJob
	seq  atomic.Int64

	used  atomic.Int64
	limit atomic.Int64

	wmu     sync.Mutex
	wcond   *sync.Cond
	wactive int
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func newJobRegistry(d *deps.Deps, lf *logFiles) *jobRegistry {
	r := &jobRegistry{d: d, lf: lf, jobs: map[string]*queryJob{}}
	r.wcond = newCond(r)

	base := "/tmp"
	instance := "default"
	if d != nil && d.Cfg != nil && d.Cfg.Aurora.MCP.ScratchDir != "" {
		base = d.Cfg.Aurora.MCP.ScratchDir
	}
	if d != nil && d.ListenAddr != "" {
		instance = sanitizeRe.ReplaceAllString(d.ListenAddr, "_")
	}
	r.dir = filepath.Join(base, "curio-aurora-diag-"+instance)
	// spills of a previous run are never referenced again
	if err := os.RemoveAll(r.dir); err != nil {
		log.Warnw("cleaning log query scratch dir", "dir", r.dir, "error", err)
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		log.Warnw("creating log query scratch dir; matched lines will not be kept", "dir", r.dir, "error", err)
	}
	go r.cleanupLoop()
	return r
}

func newCond(r *jobRegistry) *sync.Cond { return sync.NewCond(&r.wmu) }

func (r *jobRegistry) workers() int {
	if r.d != nil && r.d.Cfg != nil && r.d.Cfg.Aurora.MCP.LogScanWorkers != nil {
		if n := r.d.Cfg.Aurora.MCP.LogScanWorkers.Get(); n > 0 {
			return n
		}
	}
	return max(2, runtime.NumCPU()/4)
}

func (r *jobRegistry) acquireWorker(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		r.wmu.Lock()
		r.wcond.Broadcast()
		r.wmu.Unlock()
	})
	defer stop()
	r.wmu.Lock()
	defer r.wmu.Unlock()
	for r.wactive >= r.workers() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.wcond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.wactive++
	return nil
}

func (r *jobRegistry) releaseWorker() {
	r.wmu.Lock()
	r.wactive--
	r.wcond.Signal()
	r.wmu.Unlock()
}

// refreshLimit sets the spill budget: MaxScratchBytes, but never more than
// half the free space of the scratch filesystem.
func (r *jobRegistry) refreshLimit() {
	lim := defaultScratch
	if r.d != nil && r.d.Cfg != nil && r.d.Cfg.Aurora.MCP.MaxScratchBytes != nil {
		lim = r.d.Cfg.Aurora.MCP.MaxScratchBytes.Get()
	}
	var st unix.Statfs_t
	if err := unix.Statfs(r.dir, &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		lim = min(lim, r.used.Load()+free/2)
	} else {
		lim = 0
	}
	r.limit.Store(lim)
}

func (r *jobRegistry) reserve(n int64) bool {
	if r.used.Add(n) > r.limit.Load() {
		r.used.Add(-n)
		return false
	}
	return true
}

func (r *jobRegistry) newSpill(j *queryJob) *spillFile {
	p := filepath.Join(r.dir, fmt.Sprintf("%s-%d.spill", j.id, r.seq.Add(1)))
	s := &spillFile{path: p, job: j}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.truncated = true
		return s
	}
	s.f, s.w = f, bufio.NewWriterSize(f, 1<<20)
	return s
}

func (r *jobRegistry) dropSpill(s *spillFile) {
	s.close()
	_ = os.Remove(s.path)
	r.used.Add(-s.bytes)
}

func (r *jobRegistry) get(id string) (*queryJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, badRequest(fmt.Sprintf("unknown or expired job %q", id))
	}
	return j, nil
}

func (r *jobRegistry) remove(j *queryJob) {
	j.mu.Lock()
	spills := j.spills
	j.spills = nil
	j.mu.Unlock()
	for _, s := range spills {
		r.dropSpill(s)
	}
	delete(r.jobs, j.id)
}

func (r *jobRegistry) cleanupLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		r.cleanup()
	}
}

// cleanup drops finished jobs past their retention, and the oldest finished
// jobs over maxRetainedJobs. Jobs whose lines are being read by a chained
// job are kept.
func (r *jobRegistry) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var finished []*queryJob
	for _, j := range r.jobs {
		j.mu.Lock()
		fin, state := j.finished, j.state
		j.mu.Unlock()
		if state == jobRunning || j.refs.Load() > 0 {
			continue
		}
		if time.Since(fin) > jobRetention {
			r.remove(j)
			continue
		}
		finished = append(finished, j)
	}
	if len(finished) > maxRetainedJobs {
		sort.Slice(finished, func(a, b int) bool { return finished[a].finished.Before(finished[b].finished) })
		for _, j := range finished[:len(finished)-maxRetainedJobs] {
			r.remove(j)
		}
	}
}

func newJobID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func waitJob(ctx context.Context, j *queryJob, waitMS int) {
	wait := time.Duration(min(max(waitMS, 0), maxWaitMS)) * time.Millisecond
	if wait == 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-j.done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// ---- ops ----

func (r *jobRegistry) start(ctx context.Context, req QueryRequest) (*JobStatus, error) {
	q, err := compileQuery(req)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	running := 0
	for _, j := range r.jobs {
		j.mu.Lock()
		if j.state == jobRunning {
			running++
		}
		j.mu.Unlock()
	}
	r.mu.Unlock()
	if running >= maxRunningJobs {
		return nil, badRequest(fmt.Sprintf("%d log query jobs already running on this node; wait for or cancel one", running))
	}
	r.cleanup()
	r.refreshLimit()

	maxRun := defaultJobMaxRun
	if r.d != nil && r.d.Cfg != nil && r.d.Cfg.Aurora.MCP.MaxLogJobRuntime != nil {
		if v := r.d.Cfg.Aurora.MCP.MaxLogJobRuntime.Get(); v > 0 {
			maxRun = v
		}
	}
	jctx, cancel := context.WithTimeout(context.Background(), maxRun)

	j := &queryJob{
		id:        newJobID(),
		reg:       r,
		q:         q,
		req:       req,
		created:   time.Now(),
		cancel:    cancel,
		spillOn:   !req.NoSpill,
		scanLimit: req.MaxScanBytes,
		state:     jobRunning,
		done:      make(chan struct{}),
	}
	if err := j.plan(jctx, r.lf, req); err != nil {
		cancel()
		if j.parent != nil {
			j.parent.refs.Add(-1)
		}
		return nil, err
	}

	r.mu.Lock()
	r.jobs[j.id] = j
	r.mu.Unlock()

	go func() {
		defer cancel()
		j.run(jctx)
	}()

	waitJob(ctx, j, req.WaitMS)
	return j.status(j.isFinished()), nil
}

func (j *queryJob) isFinished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

type JobRequest struct {
	ID     string
	WaitMS int `json:",omitempty"`
}

func (r *jobRegistry) status(ctx context.Context, req JobRequest) (*JobStatus, error) {
	j, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	waitJob(ctx, j, req.WaitMS)
	return j.status(j.isFinished()), nil
}

func (r *jobRegistry) cancelJob(_ context.Context, req JobRequest) (*JobStatus, error) {
	j, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	j.cancel()
	<-j.done
	return j.status(false), nil
}

type JobsResponse struct {
	Jobs         []*JobStatus
	ScratchUsed  int64
	ScratchLimit int64
	Workers      int
}

func (r *jobRegistry) list(_ context.Context, _ struct{}) (*JobsResponse, error) {
	r.mu.Lock()
	jobs := make([]*queryJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].created.After(jobs[b].created) })
	r.refreshLimit()
	out := &JobsResponse{ScratchUsed: r.used.Load(), ScratchLimit: r.limit.Load(), Workers: r.workers()}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, j.status(false))
	}
	return out, nil
}

type JobLinesRequest struct {
	ID       string
	From     int64
	Count    int `json:",omitempty"`
	MaxBytes int `json:",omitempty"`
}

type JobLinesResponse struct {
	ID    string
	Total int64
	From  int64
	// Next is the record index to continue from; equal to Total at the end.
	Next  int64
	Lines []QueryMatch
}

// lines pages through the matched lines a job kept.
func (r *jobRegistry) lines(ctx context.Context, req JobLinesRequest) (*JobLinesResponse, error) {
	j, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	if !j.isFinished() {
		return nil, badRequest("job is still running")
	}
	if req.From < 0 {
		return nil, badRequest("invalid from")
	}
	count := clampInt(req.Count, defaultPageLines, maxPageLines)
	maxBytes := clampInt(req.MaxBytes, defaultPageBytes, maxPageBytes)

	j.mu.Lock()
	spills := append([]*spillFile(nil), j.spills...)
	j.mu.Unlock()

	resp := &JobLinesResponse{ID: j.id, From: req.From}
	for _, s := range spills {
		resp.Total += s.records
	}
	next := req.From // global index of the next record to return
	pos := int64(0)  // global index of the first record of the current spill
	size := 0
	for _, s := range spills {
		if len(resp.Lines) >= count || size >= maxBytes || ctx.Err() != nil {
			break
		}
		if next >= pos+s.records {
			pos += s.records
			continue
		}
		skip := next - pos
		f, err := os.Open(s.path)
		if err != nil {
			return nil, err
		}
		if ii := skip / spillIndexEvery; ii > 0 && ii < int64(len(s.index)) {
			if _, err := f.Seek(s.index[ii], io.SeekStart); err != nil {
				_ = f.Close()
				return nil, err
			}
			skip -= ii * spillIndexEvery
		}
		lr := newLineReader(f, 0)
		for len(resp.Lines) < count && size < maxBytes {
			rec, _, err := lr.next()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					_ = f.Close()
					return nil, err
				}
				break
			}
			if skip > 0 {
				skip--
				continue
			}
			fi, off, line, ok := parseSpillRecord(rec)
			if !ok {
				continue
			}
			name := ""
			if fi < len(j.fileNames) {
				name = j.fileNames[fi]
			}
			m := QueryMatch{File: name, Offset: off, Line: truncLine(line, maxPageLineOut)}
			resp.Lines = append(resp.Lines, m)
			size += len(m.Line) + 64
			next++
		}
		_ = f.Close()
		pos += s.records
	}
	resp.Next = req.From + int64(len(resp.Lines))
	return resp, nil
}
