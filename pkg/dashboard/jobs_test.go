package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitDone(t *testing.T, j *job) jobView {
	t.Helper()
	select {
	case <-j.done:
		return j.view()
	case <-t.Context().Done():
		t.Fatal("job did not finish")
		return jobView{}
	}
}

func TestRunner(t *testing.T) {
	var r runner
	t.Cleanup(r.close)

	release := make(chan struct{})
	j, err := r.start(jobEnrich, "a.b", "because", "pk", time.Minute, func(ctx context.Context) (string, error) {
		<-release
		return "1 ok\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.start(jobEnrich, "a.b", "", "pk", time.Minute, nil)
	if err != nil || again != j {
		t.Fatalf("second start of a running job: %v %v", again, err)
	}
	if v := j.view(); !v.Running || v.Note != "because" {
		t.Fatalf("running view %+v", v)
	}
	close(release)
	if v := waitDone(t, j); v.Output != "1 ok" || v.Err != "" || v.Running {
		t.Fatalf("done view %+v", v)
	}

	failed, err := r.start(jobEnrich, "c.d", "", "pk", time.Minute, func(ctx context.Context) (string, error) {
		return "partial", errors.New("model down")
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := waitDone(t, failed); v.Err != "model down" || v.Output != "partial" {
		t.Fatalf("failed view %+v", v)
	}

	got := r.list(func(kind, target string) bool { return target == "a.b" })
	if len(got) != 1 || got[0].ID != j.id {
		t.Fatalf("list %+v", got)
	}
}

func TestRunnerSealIsSingle(t *testing.T) {
	var r runner
	t.Cleanup(r.close)
	_, err := r.start(jobSeal, "", "", "pk", time.Minute, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(jobSeal, "org.x", "", "pk", time.Minute, nil); !errors.Is(err, errBusy) {
		t.Fatalf("second seal: %v", err)
	}
}

func TestRunnerStopAndClose(t *testing.T) {
	var r runner
	j, err := r.start(jobEnrich, "a.b", "", "pk", time.Minute, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.stop(j.id); err != nil {
		t.Fatal(err)
	}
	if v := waitDone(t, j); !v.Stopped || v.Err == "" {
		t.Fatalf("stopped view %+v", v)
	}
	if err := r.stop(999); !errors.Is(err, errNoJob) {
		t.Fatalf("stop unknown: %v", err)
	}
	r.close()
	if _, err := r.start(jobEnrich, "x.y", "", "pk", time.Minute, nil); !errors.Is(err, errClosed) {
		t.Fatalf("start after close: %v", err)
	}
}

func TestRunnerBounds(t *testing.T) {
	var r runner
	t.Cleanup(r.close)
	block := func(ctx context.Context) (string, error) { <-ctx.Done(); return "", nil }
	for i := range maxRunning {
		if _, err := r.start(jobEnrich, string(rune('a'+i)), "", "", time.Minute, block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.start(jobEnrich, "z", "", "", time.Minute, block); !errors.Is(err, errTooMany) {
		t.Fatalf("over the limit: %v", err)
	}
}
