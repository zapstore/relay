package dashboard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pippellia-btc/nwt"
	"github.com/zapstore/relay/pkg/catalog"
)

// SealRequest is one seal. HasFrom writes the bundle from that snapshot to
// the current one. Otherwise the next snapshot is sealed.
// Filter is an optional app-id substring and is only used for the next snapshot.
type SealRequest struct {
	Filter  string
	From    int64
	To      int64
	HasFrom bool
}

// SealFunc writes a bundle and returns the line shown on the job.
type SealFunc func(ctx context.Context, req SealRequest) (string, error)

// sealTimeout covers enriching every changed app before the bundle is signed.
const sealTimeout = 15 * time.Minute

type bundleRow struct {
	Name    string
	From    int64
	To      int64
	Size    string
	When    string
	Changed time.Time
}

type bundlesPage struct {
	IsAdmin     bool
	CanSeal     bool
	Snapshot    int64
	Interval    string
	IntervalOff bool
	Bundles     []bundleRow
	Jobs        []jobView
	Now         time.Time
}

func (d *T) bundlesPage(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	rows, err := listBundles(d.dataDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	snap, err := catalog.LatestSnap(d.dataDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	admin := d.auth.IsAdmin(token)
	d.render(w, "bundles", bundlesPage{
		IsAdmin:     admin,
		CanSeal:     admin && d.seal != nil,
		Snapshot:    snap,
		Interval:    formatInterval(d.config.BundleInterval),
		IntervalOff: d.config.BundleInterval <= 0,
		Bundles:     rows,
		Jobs:        withStop(d.jobs.list(func(kind, _ string) bool { return kind == jobSeal }), admin),
		Now:         time.Now(),
	})
}

func (d *T) sealBundle(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if d.seal == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("sealing is not available on this relay"))
		return
	}
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	filter := strings.TrimSpace(r.FormValue("filter"))
	from, set, err := bundleFrom(r.FormValue("from"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if set && filter != "" {
		fail(w, http.StatusBadRequest, errors.New("leave the filter empty when from is set"))
		return
	}
	var to int64
	if set {
		to, err = catalog.LatestSnap(d.dataDir)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if to == 0 || from >= to {
			fail(w, http.StatusBadRequest, errors.New("from must be older than the current snapshot"))
			return
		}
		if err := snapshotExists(d.dataDir, from); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	target := filter
	if set {
		target = strconv.FormatInt(from, 10) + "-" + strconv.FormatInt(to, 10)
	}
	req := SealRequest{Filter: filter, From: from, To: to, HasFrom: set}
	_, err = d.jobs.start(jobSeal, target, note(r), token.Signer, sealTimeout, func(ctx context.Context) (string, error) {
		return d.seal(ctx, req)
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	d.render(w, "jobs", withStop(d.jobs.list(func(kind, _ string) bool { return kind == jobSeal }), true))
}

func (d *T) deleteBundle(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	from, to := r.PathValue("from"), r.PathValue("to")
	if err := removeBundle(d.dataDir, from, to); err != nil {
		if errors.Is(err, errBadBundle) {
			fail(w, http.StatusBadRequest, err)
			return
		}
		fail(w, http.StatusInternalServerError, err)
		return
	}
	slog.Info("dashboard: bundle deleted", "bundle", from+"-"+to, "by", token.Signer)
	d.bundlesPage(w, r, token)
}

var errBadBundle = errors.New("invalid bundle")

// removeBundle deletes one bundle file. The name must be two plain integers.
// A missing file is success.
func removeBundle(data, from, to string) error {
	f, t, ok := parseBundleName(from + "-" + to + ".tar.zst")
	if !ok {
		return errBadBundle
	}
	err := os.Remove(catalog.BundlePath(data, f, t))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// bundleFrom parses an optional starting snapshot. Empty means the next snapshot.
func bundleFrom(raw string) (from int64, set bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	from, err = strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(from, 10) != raw || from < 0 {
		return 0, false, errors.New("from must be a snapshot")
	}
	return from, true, nil
}

func snapshotExists(data string, n int64) error {
	if n == 0 {
		return nil
	}
	if _, err := os.Stat(catalog.SnapPath(data, n)); err != nil {
		return fmt.Errorf("snapshot %d is not on this relay", n)
	}
	return nil
}

// formatInterval is a duration without trailing zero units: 5m0s is 5m.
// Zero is empty; the page says the tick is disabled.
func formatInterval(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	s = strings.TrimSuffix(s, "0m")
	return s
}

func listBundles(data string) ([]bundleRow, error) {
	entries, err := os.ReadDir(catalog.BundleDir(data))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	rows := make([]bundleRow, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		from, to, ok := parseBundleName(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		rows = append(rows, bundleRow{
			Name:    strconv.FormatInt(from, 10) + "-" + strconv.FormatInt(to, 10),
			From:    from,
			To:      to,
			Size:    formatSize(info.Size()),
			When:    info.ModTime().UTC().Format("2006-01-02 15:04"),
			Changed: info.ModTime(),
		})
	}
	slices.SortFunc(rows, func(a, b bundleRow) int {
		if a.To != b.To {
			return cmp.Compare(b.To, a.To)
		}
		return cmp.Compare(b.From, a.From)
	})
	return rows, nil
}

func parseBundleName(name string) (int64, int64, bool) {
	base, ok := strings.CutSuffix(name, ".tar.zst")
	if !ok {
		return 0, 0, false
	}
	left, right, ok := strings.Cut(base, "-")
	if !ok || strings.Contains(right, "-") {
		return 0, 0, false
	}
	from, err1 := strconv.ParseInt(left, 10, 64)
	to, err2 := strconv.ParseInt(right, 10, 64)
	if err1 != nil || err2 != nil || from < 0 || to < 1 {
		return 0, 0, false
	}
	if strconv.FormatInt(from, 10) != left || strconv.FormatInt(to, 10) != right {
		return 0, 0, false
	}
	return from, to, true
}

// SealResult is the line for a next-snapshot seal.
func SealResult(before, after int64) string {
	if after == before {
		if after == 0 {
			return "Catalog unchanged"
		}
		return fmt.Sprintf("Catalog unchanged at snapshot %d", after)
	}
	return fmt.Sprintf("Sealed bundle %d-%d", before, after)
}
