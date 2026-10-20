package control

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	jobEnrich = "enrich"
	jobSeal   = "seal"

	// maxJobs is how many jobs, running or finished, the relay remembers.
	maxJobs = 50
	// maxRunning bounds concurrent jobs. Each enrich downloads an APK and calls the model.
	maxRunning = 4
)

var (
	errBusy    = errors.New("a bundle is already being sealed")
	errClosed  = errors.New("the relay is shutting down")
	errNoJob   = errors.New("job not found")
	errTooMany = errors.New("too many jobs are running, wait for one to finish")
)

// job is one long write started on the socket.
type job struct {
	id      int64
	kind    string
	target  string
	note    string
	by      string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	finished time.Time
	output   string
	err      string
	stopped  bool
}

// JobView is a job as the jobs op reports it. Times are unix seconds; finished is 0 while running.
type JobView struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	Note     string `json:"note"`
	By       string `json:"by"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	Running  bool   `json:"running"`
	Stopped  bool   `json:"stopped"`
	Output   string `json:"output"`
	Error    string `json:"error"`
}

func (j *job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := JobView{
		ID: j.id, Kind: j.kind, Target: j.target, Note: j.note, By: j.by,
		Started: j.started.Unix(), Running: j.finished.IsZero(),
		Stopped: j.stopped, Output: j.output, Error: j.err,
	}
	if !v.Running {
		v.Finished = j.finished.Unix()
	}
	return v
}

func (j *job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished.IsZero()
}

func (j *job) finish(out string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finished = time.Now()
	j.output = strings.TrimRight(out, "\n")
	if err != nil {
		j.err = err.Error()
	}
	close(j.done)
}

// runner owns the job goroutines. The zero value is ready to use.
// shortcut: jobs live in memory and are lost on restart; admin_log keeps who started what.
type runner struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	next   int64
	jobs   []*job // newest first
	closed bool
}

// start runs fn in the background, or returns the job already running for
// the same kind and target. Only one seal runs at a time.
func (r *runner) start(kind, target, note, by string, timeout time.Duration, fn func(context.Context) (string, error)) (*job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	running := 0
	for _, j := range r.jobs {
		if !j.running() {
			continue
		}
		running++
		if j.kind == kind && (j.target == target || kind == jobSeal) {
			if kind == jobSeal && j.target != target {
				return nil, errBusy
			}
			return j, nil
		}
	}
	if running >= maxRunning {
		return nil, errTooMany
	}

	// A job outlives the connection that started it; close and stop cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	r.next++
	j := &job{
		id: r.next, kind: kind, target: target, note: note, by: by,
		started: time.Now(), cancel: cancel, done: make(chan struct{}),
	}
	r.jobs = slices.Insert(r.jobs, 0, j)
	r.trim()

	slog.Info("control: job started", "job", j.id, "kind", kind, "target", target, "by", by, "note", note)
	r.wg.Go(func() {
		defer cancel()
		out, err := fn(ctx)
		j.finish(out, err)
		slog.Info("control: job finished", "job", j.id, "kind", kind, "target", target, "error", err)
	})
	return j, nil
}

// trim drops the oldest finished jobs beyond maxJobs. Running jobs stay.
func (r *runner) trim() {
	for i := len(r.jobs) - 1; i >= 0 && len(r.jobs) > maxJobs; i-- {
		if !r.jobs[i].running() {
			r.jobs = slices.Delete(r.jobs, i, i+1)
		}
	}
}

// list returns views of the jobs that match, newest first.
func (r *runner) list(kind, target string) []JobView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []JobView{}
	for _, j := range r.jobs {
		if (kind == "" || j.kind == kind) && (target == "" || j.target == target) {
			out = append(out, j.view())
		}
	}
	return out
}

func (r *runner) stop(id int64) error {
	r.mu.Lock()
	idx := slices.IndexFunc(r.jobs, func(j *job) bool { return j.id == id })
	var j *job
	if idx >= 0 {
		j = r.jobs[idx]
	}
	r.mu.Unlock()
	if j == nil {
		return errNoJob
	}
	j.mu.Lock()
	if j.finished.IsZero() {
		j.stopped = true
	}
	j.mu.Unlock()
	j.cancel()
	return nil
}

// close cancels running jobs and waits for them.
func (r *runner) close() {
	r.mu.Lock()
	r.closed = true
	for _, j := range r.jobs {
		j.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
