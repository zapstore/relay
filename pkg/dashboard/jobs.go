package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pippellia-btc/nwt"
)

const (
	jobEnrich = "enrich"
	jobSeal   = "seal"

	// maxJobs is how many jobs, running or finished, the dashboard remembers.
	maxJobs = 50
	// maxNote bounds the operator note stored with a job.
	maxNote = 280
)

var (
	errBusy    = errors.New("a bundle is already being sealed")
	errClosed  = errors.New("the dashboard is shutting down")
	errNoJob   = errors.New("job not found")
	errTooMany = errors.New("too many jobs are running, wait for one to finish")
)

// maxRunning bounds concurrent jobs. Each enrich downloads an APK and calls the model.
const maxRunning = 4

// job is one long action started from the dashboard.
type job struct {
	id      int64
	kind    string
	target  string
	note    string
	by      string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{} // closed when the job finishes

	mu       sync.Mutex
	finished time.Time
	output   string
	err      string
	stopped  bool
}

// jobView is a snapshot of a job for a template.
type jobView struct {
	ID       int64
	Kind     string
	Target   string
	Note     string
	By       string
	Started  time.Time
	Finished time.Time
	Running  bool
	Stopped  bool
	Output   string
	Err      string
	Elapsed  string
	CanStop  bool
}

func (j *job) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{
		ID: j.id, Kind: j.kind, Target: j.target, Note: j.note, By: j.by,
		Started: j.started, Finished: j.finished, Running: j.finished.IsZero(),
		Stopped: j.stopped, Output: j.output, Err: j.err,
	}
	end := j.finished
	if v.Running {
		end = time.Now()
	}
	v.Elapsed = end.Sub(j.started).Round(time.Second).String()
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
// shortcut: jobs live in memory and are lost on restart; persist them if operators need history.
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

	// A job outlives the request that started it; close and stop cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	r.next++
	j := &job{
		id: r.next, kind: kind, target: target, note: note, by: by,
		started: time.Now(), cancel: cancel, done: make(chan struct{}),
	}
	r.jobs = slices.Insert(r.jobs, 0, j)
	r.trim()

	slog.Info("dashboard: job started", "job", j.id, "kind", kind, "target", target, "by", by, "note", note)
	r.wg.Go(func() {
		defer cancel()
		out, err := fn(ctx)
		j.finish(out, err)
		slog.Info("dashboard: job finished", "job", j.id, "kind", kind, "target", target, "error", err)
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

func (r *runner) get(id int64) (*job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.id == id {
			return j, true
		}
	}
	return nil, false
}

// list returns views of the jobs that match, newest first.
func (r *runner) list(match func(kind, target string) bool) []jobView {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []jobView
	for _, j := range r.jobs {
		if match(j.kind, j.target) {
			out = append(out, j.view())
		}
	}
	return out
}

func (r *runner) stop(id int64) error {
	j, ok := r.get(id)
	if !ok {
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

func note(r *http.Request) string {
	n := strings.Join(strings.Fields(r.FormValue("note")), " ")
	if len([]rune(n)) > maxNote {
		n = string([]rune(n)[:maxNote])
	}
	return n
}

func (d *T) jobStatus(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	j, ok := d.jobFromPath(w, r)
	if !ok {
		return
	}
	v := j.view()
	// The polling panel asks with poll=1; tell the page once so it reloads its data.
	if !v.Running && r.URL.Query().Get("poll") == "1" {
		w.Header().Set("HX-Trigger", "job-done")
	}
	v.CanStop = d.auth.IsAdmin(token)
	d.render(w, "job", v)
}

func (d *T) cancelJob(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	j, ok := d.jobFromPath(w, r)
	if !ok {
		return
	}
	if err := d.jobs.stop(j.id); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	slog.Info("dashboard: job cancelled", "job", j.id, "by", token.Signer)
	v := j.view()
	v.CanStop = true
	d.render(w, "job", v)
}

func (d *T) jobFromPath(w http.ResponseWriter, r *http.Request) (*job, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid job id"))
		return nil, false
	}
	j, ok := d.jobs.get(id)
	if !ok {
		fail(w, http.StatusNotFound, errNoJob)
		return nil, false
	}
	return j, true
}

func withStop(views []jobView, admin bool) []jobView {
	for i := range views {
		views[i].CanStop = admin
	}
	return views
}
