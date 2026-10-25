package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/events"
)

const (
	// readRetry is how long an APK that could not be fetched waits before
	// another try.
	readRetry = 10 * time.Minute
	// readTimeout bounds one APK read, including a download.
	readTimeout   = 5 * time.Minute
	readQueueSize = 16
)

var (
	errNoAPK     = errors.New("the apk is not in blossom or the apk cache, and has no url")
	errAPKHash   = errors.New("the downloaded apk does not match its hash")
	errAPKTooBig = errors.New("apk exceeds the size limit")
)

// apkJob asks for the certificates of the APK with sha256 blob. url is where
// to download it when Blossom does not have it.
type apkJob struct {
	blob string
	url  string
}

// readAPKResult is a signing chain read during this process. A restart reads
// the file again. delivered is the refusal for a store's delivery mark,
// which only a listing without a C1 proof gets.
type readAPKResult struct {
	chain     signing
	readErr   string
	delivered string
}

// apkReader reads APK certificates one at a time, off the event path.
type apkReader struct {
	jobs chan apkJob

	mu      sync.Mutex
	queued  map[string]struct{}
	retryAt map[string]time.Time
	read    map[string]readAPKResult
}

func newAPKReader() *apkReader {
	return &apkReader{
		jobs:    make(chan apkJob, readQueueSize),
		queued:  map[string]struct{}{},
		retryAt: map[string]time.Time{},
		read:    map[string]readAPKResult{},
	}
}

func (a *apkReader) remember(blob string, res readAPKResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.read[blob] = res
}

// delivered returns the delivery mark refusal of a read APK, or "".
func (a *apkReader) delivered(blob string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.read[blob].delivered
}

func (a *apkReader) cached(blob string) (signing, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hit, ok := a.read[blob]
	return hit.chain, hit.readErr, ok
}

// request queues a read unless the blob is queued or waiting to retry.
func (a *apkReader) request(job apkJob) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.queued[job.blob]; ok {
		return
	}
	if time.Now().Before(a.retryAt[job.blob]) {
		return
	}
	select {
	case a.jobs <- job:
		a.queued[job.blob] = struct{}{}
	default:
		slog.Warn("apk reader queue is full", "blob", job.blob)
	}
}

func (a *apkReader) done(blob string, failed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.queued, blob)
	if failed {
		a.retryAt[blob] = time.Now().Add(readRetry)
	} else {
		delete(a.retryAt, blob)
	}
}

func (r *T) runAPKReader(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-r.apks.jobs:
			err := r.readAPKJob(ctx, job)
			r.apks.done(job.blob, err != nil)
			if err != nil {
				slog.Warn("apk reader: apk unavailable", "blob", job.blob, "error", err)
				continue
			}
			if err := r.reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("apk reader: reconcile failed", "error", err)
			}
		}
	}
}

// readAPKJob reads one APK and remembers its certificates until the process
// exits. A file that is not a verifiable APK, or that a store signed, is
// remembered with the refusal, since the same bytes will not pass later. An
// APK that could not be fetched returns an error.
func (r *T) readAPKJob(ctx context.Context, job apkJob) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	body, cleanup, err := r.openAPK(ctx, job)
	if err != nil {
		return err
	}
	defer cleanup()
	chain, readErr := r.readAPK(body)
	var store *thirdPartyError
	if errors.As(readErr, &store) && store.delivery && len(chain) > 0 {
		slog.Info("apk reader: read apk", "blob", job.blob, "signer", chain.signer(), "delivered_by", store.label)
		r.apks.remember(job.blob, readAPKResult{chain: chain, delivered: store.Error()})
		return nil
	}
	if readErr != nil {
		reason := "the APK could not be verified: " + readErr.Error()
		if store != nil {
			reason = store.Error()
		}
		slog.Warn("apk reader: apk refused", "blob", job.blob, "reason", reason)
		r.apks.remember(job.blob, readAPKResult{readErr: reason})
		return nil
	}
	slog.Info("apk reader: read apk", "blob", job.blob, "signer", chain.signer())
	r.apks.remember(job.blob, readAPKResult{chain: chain})
	return nil
}

// openAPK returns the APK from Blossom, buffer zone first, else from the APK
// cache, else from job.url.
func (r *T) openAPK(ctx context.Context, job apkJob) (io.ReadSeeker, func(), error) {
	hash, err := blossom.ParseHash(job.blob)
	if err != nil {
		return nil, nil, err
	}
	has, err := r.blossom.Has(ctx, hash)
	if err != nil {
		return nil, nil, err
	}
	if has {
		body, err := r.blossom.Open(ctx, hash)
		if err != nil {
			return nil, nil, err
		}
		if f, ok := body.(io.ReadSeeker); ok {
			return f, func() { body.Close() }, nil
		}
		defer body.Close()
		return tempAPK(body, job.blob)
	}
	if body, cleanup, ok := r.cachedAPK(job.blob); ok {
		return body, cleanup, nil
	}
	if job.url == "" {
		return nil, nil, errNoAPK
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.url, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := r.fetch.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("fetch apk: status %d", res.StatusCode)
	}
	return tempAPK(res.Body, job.blob)
}

// cachedAPK opens <APKCacheDir>/<blob>.apk when its sha256 is blob. The
// indexer writes that directory; the relay only reads it.
func (r *T) cachedAPK(blob string) (io.ReadSeeker, func(), bool) {
	if r.config.APKCacheDir == "" {
		return nil, nil, false
	}
	f, err := os.Open(filepath.Join(r.config.APKCacheDir, blob+".apk"))
	if err != nil {
		return nil, nil, false
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(f, maxAPK+1))
	if err != nil || n > maxAPK || hex.EncodeToString(hasher.Sum(nil)) != blob {
		f.Close()
		slog.Warn("apk reader: cached apk does not match its hash", "blob", blob)
		return nil, nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, false
	}
	return f, func() { f.Close() }, true
}

// tempAPK copies an APK to a temporary file and checks its sha256.
func tempAPK(r io.Reader, blob string) (io.ReadSeeker, func(), error) {
	f, err := os.CreateTemp("", "relay-apk-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, maxAPK+1))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if n > maxAPK {
		cleanup()
		return nil, nil, errAPKTooBig
	}
	if hex.EncodeToString(hasher.Sum(nil)) != blob {
		cleanup()
		return nil, nil, errAPKHash
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, err
	}
	return f, cleanup, nil
}

// apkSigning returns the chain of the APK with sha256 blob. ok is false until
// the APK has been read during this process; the read is then queued. readErr
// is set when the file is not a verifiable APK.
func (r *T) apkSigning(blob, url string) (chain signing, readErr string, ok bool) {
	chain, readErr, ok = r.apks.cached(blob)
	if !ok {
		r.apks.request(apkJob{blob: blob, url: url})
		return nil, "", false
	}
	return chain, readErr, true
}

// assetURL is an https download URL named by an asset, for an APK that
// Blossom does not have. A URL on this relay's CDN is skipped: that file would
// be in Blossom.
func (r *T) assetURL(tags nostr.Tags) string {
	for _, raw := range events.FindAll(tags, "url") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			continue
		}
		if slices.ContainsFunc(r.config.BlobHosts, func(h string) bool { return h != "" && strings.EqualFold(u.Hostname(), h) }) {
			continue
		}
		return raw
	}
	return ""
}
