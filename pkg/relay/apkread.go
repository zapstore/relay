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
	"github.com/zapstore/relay/pkg/relay/store"
)

const (
	// An APK that could not be fetched is fetched again after readBackoff,
	// doubling up to readBackoffMax, until it is read or nothing waits on it.
	readBackoff    = 15 * time.Second
	readBackoffMax = 5 * time.Minute
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
	blob  string
	url   string
	tries int
}

// readAPKResult is a signing chain read during this process. A restart reads
// the file again.
type readAPKResult struct {
	chain   signing
	readErr string
}

// apkReader reads APK certificates one at a time, off the event path.
type apkReader struct {
	jobs chan apkJob

	mu     sync.Mutex
	queued map[string]struct{} // queued, being read, or waiting to fetch again
	read   map[string]readAPKResult
}

func newAPKReader() *apkReader {
	return &apkReader{
		jobs:   make(chan apkJob, readQueueSize),
		queued: map[string]struct{}{},
		read:   map[string]readAPKResult{},
	}
}

func (a *apkReader) remember(blob string, res readAPKResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.read[blob] = res
}

func (a *apkReader) cached(blob string) (signing, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hit, ok := a.read[blob]
	return hit.chain, hit.readErr, ok
}

// request queues a read unless the blob is already queued. A full queue
// holds the job until there is room; it is never dropped.
func (a *apkReader) request(job apkJob) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.queued[job.blob]; ok {
		return
	}
	a.queued[job.blob] = struct{}{}
	select {
	case a.jobs <- job:
	default:
		go func() { a.jobs <- job }()
	}
}

func (a *apkReader) done(blob string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.queued, blob)
}

func (r *T) runAPKReader(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-r.apks.jobs:
			if err := r.readAPKJob(ctx, job); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("apk reader: apk unavailable, fetching again", "blob", job.blob, "tries", job.tries+1, "error", err)
				go r.fetchAgain(ctx, job)
				continue
			}
			r.apks.done(job.blob)
			if err := r.promoteWaitingOn(ctx, job.blob); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("apk reader: promote failed", "blob", job.blob, "error", err)
			}
		}
	}
}

// fetchAgain queues a failed read again after its backoff, unless nothing
// waits on the blob any more.
func (r *T) fetchAgain(ctx context.Context, job apkJob) {
	job.tries++
	delay := readBackoffMax
	if job.tries < 6 {
		delay = min(readBackoff<<(job.tries-1), readBackoffMax)
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	waited, err := r.waitedOn(ctx, job.blob)
	if err == nil && !waited {
		r.apks.done(job.blob)
		return
	}
	select {
	case <-ctx.Done():
	case r.apks.jobs <- job:
	}
}

// waitedOn reports a pending asset that names blob, or a pending claim for
// the certificate of a stored asset that names it.
func (r *T) waitedOn(ctx context.Context, blob string) (bool, error) {
	assets, err := r.store.PendingTagged(ctx, events.KindAsset, "x", blob)
	if err != nil || len(assets) > 0 {
		return len(assets) > 0, err
	}
	certs, err := storedAssetCerts(ctx, r.store, blob)
	if err != nil {
		return false, err
	}
	for _, cert := range certs {
		proofs, err := r.store.PendingTagged(ctx, events.KindIdentityProof, "d", cert)
		if err != nil || len(proofs) > 0 {
			return len(proofs) > 0, err
		}
	}
	return false, nil
}

// promoteWaitingOn promotes what waited on the read of blob: the pending
// listings whose asset names it, and the pending claims for the certificate
// of a stored asset that names it.
func (r *T) promoteWaitingOn(ctx context.Context, blob string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	assets, err := r.store.PendingTagged(ctx, events.KindAsset, "x", blob)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, asset := range assets {
		appID, ok := listingAppID(&asset)
		if !ok {
			continue
		}
		key := asset.PubKey + ":" + appID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, err := r.promoteListing(ctx, asset.PubKey, appID, ""); err != nil {
			return err
		}
	}

	certs, err := storedAssetCerts(ctx, r.store, blob)
	if err != nil {
		return err
	}
	for _, cert := range certs {
		proofs, err := r.store.PendingTagged(ctx, events.KindIdentityProof, "d", cert)
		if err != nil {
			return err
		}
		for _, proof := range proofs {
			if _, err := r.promoteClaim(ctx, proof, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// storedAssetCerts returns the certificates named by stored assets of blob.
func storedAssetCerts(ctx context.Context, db store.T, blob string) ([]string, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT DISTINCT lower(c.value) FROM tags x
		JOIN events e ON e.id = x.event_id AND e.kind = ?
		JOIN tags c ON c.event_id = e.id AND c.key = 'apk_certificate_hash'
		WHERE x.key = 'x' AND x.value = ?
	`, events.KindAsset, blob)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var certs []string
	for rows.Next() {
		var cert string
		if err := rows.Scan(&cert); err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	return certs, rows.Err()
}

// readAPKJob reads one APK and remembers its certificates until the process
// exits. A file that is not a verifiable APK is remembered with the refusal,
// since the same bytes will not pass later. An APK that could not be fetched
// returns an error.
func (r *T) readAPKJob(ctx context.Context, job apkJob) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	body, cleanup, err := r.openAPK(ctx, job)
	if err != nil {
		// The same bytes will not pass later, so this is a refusal, not a
		// download to try again.
		if errors.Is(err, errAPKTooBig) {
			slog.Warn("apk reader: apk refused", "blob", job.blob, "reason", err.Error())
			r.apks.remember(job.blob, readAPKResult{readErr: err.Error()})
			return nil
		}
		return err
	}
	defer cleanup()
	chain, readErr := r.readAPK(body)
	if readErr != nil {
		reason := "the APK could not be verified: " + readErr.Error()
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
			n, err := seekEnd(f)
			if err != nil {
				body.Close()
				return nil, nil, err
			}
			if n > maxAPK {
				body.Close()
				return nil, nil, errAPKTooBig
			}
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
	if r.config.APKCacheDir != "" {
		return keepAPK(res.Body, r.config.APKCacheDir, job.blob)
	}
	return tempAPK(res.Body, job.blob)
}

// keepAPK writes a downloaded APK to <dir>/<blob>.apk after checking its
// sha256, so the enricher reads it there instead of downloading it again.
func keepAPK(r io.Reader, dir, blob string) (io.ReadSeeker, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(dir, ".relay-apk-*")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (io.ReadSeeker, func(), error) {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, maxAPK+1))
	if err != nil {
		return fail(err)
	}
	if n > maxAPK {
		return fail(errAPKTooBig)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != blob {
		return fail(errAPKHash)
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, blob+".apk")); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

// seekEnd returns the size of a seekable APK and rewinds it for the reader.
func seekEnd(f io.ReadSeeker) (int64, error) {
	n, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return n, nil
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
