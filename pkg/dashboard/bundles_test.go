package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	astore "github.com/zapstore/relay/pkg/analytics/store"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/relay"
)

func TestListBundles(t *testing.T) {
	data := t.TempDir()
	dir := catalog.BundleDir(data)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0-1.tar.zst", "1-2.tar.zst", "notes.txt", "1-02.tar.zst"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := listBundles(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "1-2" || rows[1].Name != "0-1" {
		t.Fatalf("%+v", rows)
	}
	if rows[0].Size != "1 B" {
		t.Fatalf("size %q", rows[0].Size)
	}
	if formatInterval(5*time.Minute) != "5m" || formatInterval(time.Hour) != "1h" || formatInterval(0) != "" {
		t.Fatalf("interval %q %q", formatInterval(5*time.Minute), formatInterval(time.Hour))
	}
	missing, err := listBundles(filepath.Join(t.TempDir(), "absent"))
	if err != nil || missing != nil {
		t.Fatalf("missing %v err %v", missing, err)
	}
}

func TestRemoveBundle(t *testing.T) {
	data := t.TempDir()
	dir := catalog.BundleDir(data)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "0-1.tar.zst")
	drop := filepath.Join(dir, "1-2.tar.zst")
	for _, name := range []string{keep, drop} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeBundle(data, "1", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Fatal("bundle file still present")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
	if err := removeBundle(data, "1", "2"); err != nil {
		t.Fatal("missing bundle")
	}
	if err := removeBundle(data, "../1", "2"); err != errBadBundle {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("other bundle was removed")
	}
}

func TestSealResult(t *testing.T) {
	if got := SealResult(0, 0); got != "Catalog unchanged" {
		t.Fatal(got)
	}
	if got := SealResult(4, 4); got != "Catalog unchanged at snapshot 4" {
		t.Fatal(got)
	}
	if got := SealResult(4, 5); got != "Sealed bundle 4-5" {
		t.Fatal(got)
	}
}

func TestBundleFrom(t *testing.T) {
	from, set, err := bundleFrom("  ")
	if err != nil || set || from != 0 {
		t.Fatalf("empty: %d %v %v", from, set, err)
	}
	from, set, err = bundleFrom("0")
	if err != nil || !set || from != 0 {
		t.Fatalf("zero: %d %v %v", from, set, err)
	}
	if _, _, err := bundleFrom("02"); err == nil {
		t.Fatal("leading zero")
	}
	if _, _, err := bundleFrom("-1"); err == nil {
		t.Fatal("negative")
	}
	data := t.TempDir()
	if err := snapshotExists(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := snapshotExists(data, 1); err == nil {
		t.Fatal("missing snapshot")
	}
	if err := os.MkdirAll(catalog.SnapDir(data), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.SnapPath(data, 1), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := snapshotExists(data, 1); err != nil {
		t.Fatal(err)
	}
}

func TestBundlesTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	body := render(t, d, "bundles", bundlesPage{
		IsAdmin: true, CanSeal: true, Snapshot: 2, Interval: "5m",
		Bundles: []bundleRow{{Name: "1-2", From: 1, To: 2, Size: "1 B", When: "2026-01-02 03:04", Changed: time.Now()}},
		Jobs:    []jobView{{ID: 1, Kind: jobSeal, Target: "org.signal", Output: "Sealed bundle 1-2", Finished: time.Now()}},
		Now:     time.Now(),
	})
	wantAll(t, body, "Seal bundle", `hx-post="/bundles/seal"`, `name="from"`, `name="filter"`, "1 → 2", "Sealed bundle 1-2", "Seal “org.signal”", "Done in", "BUNDLE_INTERVAL", "5m", "every 5m",
		`hx-post="/bundles/1/2"`, `hx-confirm="Delete bundle 1-2?`)
	if strings.Contains(body, `name="to"`) {
		t.Error("seal form asks for to")
	}
	if strings.Contains(body, `name="note"`) {
		t.Error("seal form asks for a note")
	}

	body = render(t, d, "bundles", bundlesPage{IntervalOff: true, Now: time.Now()})
	if strings.Contains(body, "hx-post") {
		t.Fatalf("seal form still offered\n%s", body)
	}
	wantAll(t, body, "Only admins can seal bundles", "No bundles yet", "Disabled", "Automatic sealing is off")
}

func TestBundleClients(t *testing.T) {
	rows := []astore.BundleCount{
		{BundleRequest: astore.BundleRequest{ClientVersion: "1.0"}, Count: 2},
		{BundleRequest: astore.BundleRequest{ClientVersion: "2.0.1"}, Count: 5},
		{BundleRequest: astore.BundleRequest{ClientVersion: ""}, Count: 5},
		{BundleRequest: astore.BundleRequest{ClientVersion: "2.0.1"}, Count: 1},
	}
	got := bundleClients(rows)
	if len(got) != 3 || got[0].Value != "2.0.1" || got[1].Label != "No version" || got[1].Value != "-" || got[2].Value != "1.0" {
		t.Fatalf("%+v", got)
	}
	if matchClient(" 2.0.1 ", got) != "2.0.1" || matchClient("-", got) != "-" || matchClient("9.9.9", got) != "" || matchClient("", got) != "" {
		t.Fatal("match")
	}
	kept := filterClient(rows, "2.0.1")
	if len(kept) != 2 || kept[0].Count != 5 || kept[1].Count != 1 {
		t.Fatalf("filter %+v", kept)
	}
	if statsHref(30, "") != "/stats?days=30" || statsHref(7, "2.0.1") != "/stats?days=7&v=2.0.1" || statsHref(7, "1.0+1") != "/stats?days=7&v=1.0%2B1" {
		t.Fatal(statsHref(7, "1.0+1"))
	}
}

func TestBundleSeries(t *testing.T) {
	rows := []astore.BundleCount{
		{BundleRequest: astore.BundleRequest{Day: "2026-01-02", Status: 200}, Count: 4},
		{BundleRequest: astore.BundleRequest{Day: "2026-01-02", Status: 304}, Count: 7},
		{BundleRequest: astore.BundleRequest{Day: "2026-01-02", Status: 500}, Count: 1},
		{BundleRequest: astore.BundleRequest{Day: "2026-01-03", Status: 200}, Count: 2},
	}
	reqs, sent, fresh := bundleSeries([]string{"2026-01-01", "2026-01-02", "2026-01-03"}, rows)
	if reqs[0] != 0 || sent[0] != 0 || fresh[0] != 0 {
		t.Fatalf("empty day %v %v %v", reqs, sent, fresh)
	}
	if reqs[1] != 12 || sent[1] != 4 || fresh[1] != 7 {
		t.Fatalf("mixed day reqs=%d sent=%d fresh=%d", reqs[1], sent[1], fresh[1])
	}
	if reqs[2] != 2 || sent[2] != 2 || fresh[2] != 0 {
		t.Fatalf("sent day %v %v %v", reqs, sent, fresh)
	}
}

func TestStatsTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	days := dayRange("2026-01-01", "2026-01-03")
	c := newChart("Downloads", days, []int64{0, 5, 10})
	if c.Total != 15 || c.Peak != 10 || c.Last != 10 || c.Bars[0].H != 0 || c.Bars[2].H <= c.Bars[1].H {
		t.Fatalf("chart %+v", c)
	}
	if len(c.YTicks) < 4 || c.YTicks[0].Value < c.Peak || c.YTicks[len(c.YTicks)-1].Value != 0 {
		t.Fatalf("y ticks %+v", c.YTicks)
	}
	if len(c.XTicks) != 3 || c.XTicks[0].Label != "Jan 1" || c.XTicks[1].Label != "Jan 2" || c.XTicks[2].Label != "Jan 3" {
		t.Fatalf("x ticks %+v", c.XTicks)
	}
	month := dayRange("2026-01-01", "2026-01-30")
	vals := make([]int64, len(month))
	vals[len(vals)-1] = 1834
	wide := newChart("Requests", month, vals)
	if len(wide.YTicks) < 4 || wide.YTicks[0].Value < 1834 || len(wide.XTicks) < 5 {
		t.Fatalf("month chart y=%v x=%v", wide.YTicks, wide.XTicks)
	}
	for _, tick := range wide.XTicks {
		if strings.Contains(tick.Label, "2026") {
			t.Fatalf("year in %q", tick.Label)
		}
	}
	body := render(t, d, "stats", statsPage{
		Days: 30, Ranges: statsRanges, HasAnalytics: true, Client: "2.0.1",
		Clients: []bundleClient{{Value: "2.0.1", Label: "2.0.1"}, {Value: "-", Label: "No version"}},
		Relay:   []metricChart{c},
		Bundles: []metricChart{newChart("Not modified", days, []int64{1, 0, 4})},
		Blossom: []metricChart{c},
		TopApps: []topApp{{ID: "org.signal.app", Name: "Signal", Downloads: 1200, Share: 100}},
		Catalog: catalogFacts{Apps: 3, Releases: 9, HasPolicy: true, BannedPeople: 1},
		Now:     time.Now(),
	})
	wantAll(t, body, "<svg", "<line", "2026-01-02: 5", "Jan 2", "Signal", "1,200", `aria-current="true">30 days`, "Apps listed", "<h2>Bundles</h2>", "Not modified",
		`aria-label="App version"`, `<option value="">All</option>`, `value="2.0.1" selected`, `>No version</option>`,
		`href="/stats?days=7&amp;v=2.0.1"`, `hx-get="/stats?days=30&amp;v=2.0.1"`)
	if strings.Contains(body, "chart.min.js") {
		t.Fatal("Chart.js still referenced")
	}

	off := render(t, d, "stats", statsPage{Days: 7, Ranges: statsRanges, Now: time.Now()})
	wantAll(t, off, "Analytics is off on this relay")
}
