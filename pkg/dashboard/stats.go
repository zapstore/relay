package dashboard

import (
	"cmp"
	"context"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/nwt"
	astore "github.com/zapstore/relay/pkg/analytics/store"
	"github.com/zapstore/relay/pkg/events"
)

var statsRanges = []int{7, 30, 90}

const (
	plotHeight  = 100.0
	topAppCount = 10
	// clientUnset is the stats query value for bundle requests that sent no v.
	// A real v starts with a digit, so this does not collide.
	clientUnset = "-"
)

type metricChart struct {
	Label  string
	Total  int64
	Last   int64
	Peak   int64
	Bars   []chartBar
	YTicks []yTick
	XTicks []xTick
	Width  float64
	Height float64
	From   string
	To     string
}

type chartBar struct {
	X, Y, W, H float64
	Day        string
	Value      int64
	Last       bool
}

type yTick struct {
	Value int64
	Y     float64 // svg coordinate, 0 at the top
	Top   float64 // css percent, 0 at the top
	Edge  string  // tick-start, tick-end, or empty
}

type xTick struct {
	Label string
	Left  float64
	Edge  string
}

type topApp struct {
	ID        string
	Name      string
	Downloads int
	Share     float64
}

type catalogFacts struct {
	Apps         int
	Enriched     int
	HasArtifacts bool
	Releases     int
	BannedPeople int
	BannedFiles  int
	HasPolicy    bool
	LastBundle   string
	LastBundleAt string
}

type bundleClient struct {
	Value string
	Label string
}

type statsPage struct {
	Days         int
	Ranges       []int
	Client       string
	Clients      []bundleClient
	HasAnalytics bool
	Relay        []metricChart
	Bundles      []metricChart
	Blossom      []metricChart
	TopApps      []topApp
	Installs     int
	Updates      int
	Catalog      catalogFacts
	Now          time.Time
}

func (d *T) statsPage(w http.ResponseWriter, r *http.Request, _ nwt.Token) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if !slices.Contains(statsRanges, days) {
		days = 30
	}
	page := statsPage{Days: days, Ranges: statsRanges, Now: time.Now()}

	listed, err := d.listings(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if page.Catalog, err = d.catalogFacts(ctx, len(listed)); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	if d.analytics != nil {
		page.HasAnalytics = true
		to := time.Now().UTC().Format("2006-01-02")
		from := daysAgo(days - 1)
		if page.Relay, err = d.relayCharts(ctx, from, to); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if page.Bundles, page.Clients, page.Client, err = d.bundleCharts(ctx, from, to, r.URL.Query().Get("v")); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if page.Blossom, err = d.blossomCharts(ctx, from, to); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if err := d.downloadLeaders(ctx, &page, listed, from, to); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	d.render(w, "stats", page)
}

func (d *T) catalogFacts(ctx context.Context, apps int) (catalogFacts, error) {
	f := catalogFacts{Apps: apps}
	ids, ok, err := d.artifactIDs(ctx)
	if err != nil {
		return f, err
	}
	f.Enriched, f.HasArtifacts = len(ids), ok
	if d.relay.Store != nil {
		if f.Releases, err = d.relay.Count(ctx, nostr.Filter{Kinds: []int{events.KindRelease}}); err != nil {
			return f, err
		}
	}
	if d.pol != nil {
		f.HasPolicy = true
		pubs, err := d.pol.Publishers(ctx)
		if err != nil {
			return f, err
		}
		hashes, err := d.pol.Hashes(ctx)
		if err != nil {
			return f, err
		}
		f.BannedPeople, f.BannedFiles = len(pubs), len(hashes)
	}
	bundles, err := listBundles(d.dataDir)
	if err != nil {
		return f, err
	}
	if len(bundles) > 0 {
		f.LastBundle, f.LastBundleAt = bundles[0].Name, bundles[0].When
	}
	return f, nil
}

func (d *T) relayCharts(ctx context.Context, from, to string) ([]metricChart, error) {
	rows, err := d.analytics.QueryRelayMetrics(ctx, from, to)
	if err != nil {
		return nil, err
	}
	days := dayRange(from, to)
	byDay := make(map[string]astore.RelayMetrics, len(rows))
	for _, row := range rows {
		byDay[row.Day] = row
	}
	reqs, filters, saved := make([]int64, len(days)), make([]int64, len(days)), make([]int64, len(days))
	for i, day := range days {
		m := byDay[day]
		reqs[i], filters[i], saved[i] = m.Reqs, m.Filters, m.Events
	}
	return []metricChart{
		newChart("Requests", days, reqs),
		newChart("Filters", days, filters),
		newChart("Events saved", days, saved),
	}, nil
}

func (d *T) bundleCharts(ctx context.Context, from, to, rawClient string) ([]metricChart, []bundleClient, string, error) {
	rows, err := d.analytics.QueryBundles(ctx, astore.BundleFilter{
		From: from, To: to, GroupBy: []string{"day", "status", "client_version"},
	})
	if err != nil {
		return nil, nil, "", err
	}
	clients := bundleClients(rows)
	selected := matchClient(rawClient, clients)
	days := dayRange(from, to)
	reqs, sent, fresh := bundleSeries(days, filterClient(rows, selected))
	return []metricChart{
		newChart("Requests", days, reqs),
		newChart("Sent", days, sent),
		newChart("Not modified", days, fresh),
	}, clients, selected, nil
}

// statsHref is the stats page for one day range and, when set, one app version.
func statsHref(days int, client string) string {
	href := "/stats?days=" + strconv.Itoa(days)
	if client != "" {
		href += "&v=" + url.QueryEscape(client)
	}
	return href
}

// bundleClients lists versions in this range, busiest first.
// Requests that sent no v are one entry.
func bundleClients(rows []astore.BundleCount) []bundleClient {
	counts := make(map[string]int64)
	for _, row := range rows {
		counts[row.ClientVersion] += int64(row.Count)
	}
	type item struct {
		ver string
		n   int64
	}
	items := make([]item, 0, len(counts))
	for ver, n := range counts {
		items = append(items, item{ver, n})
	}
	slices.SortFunc(items, func(a, b item) int {
		if c := cmp.Compare(b.n, a.n); c != 0 {
			return c
		}
		return cmp.Compare(a.ver, b.ver)
	})
	out := make([]bundleClient, 0, len(items))
	for _, it := range items {
		if it.ver == "" {
			out = append(out, bundleClient{Value: clientUnset, Label: "No version"})
			continue
		}
		out = append(out, bundleClient{Value: it.ver, Label: it.ver})
	}
	return out
}

// matchClient keeps raw when it is a version in this range. Anything else is all.
func matchClient(raw string, clients []bundleClient) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, c := range clients {
		if c.Value == raw {
			return raw
		}
	}
	return ""
}

func filterClient(rows []astore.BundleCount, client string) []astore.BundleCount {
	if client == "" {
		return rows
	}
	want := client
	if client == clientUnset {
		want = ""
	}
	out := make([]astore.BundleCount, 0, len(rows))
	for _, row := range rows {
		if row.ClientVersion == want {
			out = append(out, row)
		}
	}
	return out
}

// bundleSeries splits bundle responses into requests, bodies sent (200),
// and clients already current (304). Other statuses stay in requests only.
func bundleSeries(days []string, rows []astore.BundleCount) (reqs, sent, fresh []int64) {
	type dayCounts struct{ reqs, sent, fresh int64 }
	byDay := make(map[string]dayCounts, len(rows))
	for _, row := range rows {
		c := byDay[row.Day]
		n := int64(row.Count)
		c.reqs += n
		switch row.Status {
		case http.StatusOK:
			c.sent += n
		case http.StatusNotModified:
			c.fresh += n
		}
		byDay[row.Day] = c
	}
	reqs, sent, fresh = make([]int64, len(days)), make([]int64, len(days)), make([]int64, len(days))
	for i, day := range days {
		c := byDay[day]
		reqs[i], sent[i], fresh[i] = c.reqs, c.sent, c.fresh
	}
	return reqs, sent, fresh
}

func (d *T) blossomCharts(ctx context.Context, from, to string) ([]metricChart, error) {
	rows, err := d.analytics.QueryBlossomMetrics(ctx, from, to)
	if err != nil {
		return nil, err
	}
	days := dayRange(from, to)
	byDay := make(map[string]astore.BlossomMetrics, len(rows))
	for _, row := range rows {
		byDay[row.Day] = row
	}
	checks, downloads, uploads := make([]int64, len(days)), make([]int64, len(days)), make([]int64, len(days))
	for i, day := range days {
		m := byDay[day]
		checks[i], downloads[i], uploads[i] = m.Checks, m.Downloads, m.Uploads
	}
	return []metricChart{
		newChart("Checks", days, checks),
		newChart("Downloads", days, downloads),
		newChart("Uploads", days, uploads),
	}, nil
}

// newChart lays out one bar per day. The value axis rounds above the busiest
// day so the grid lines land on readable numbers.
func newChart(label string, days []string, values []int64) metricChart {
	c := metricChart{Label: label, Height: plotHeight}
	if len(days) == 0 {
		return c
	}
	c.From, c.To = days[0], days[len(days)-1]
	for _, v := range values {
		c.Total += v
		c.Peak = max(c.Peak, v)
	}
	c.Last = values[len(values)-1]
	axisMax, step := yScale(c.Peak)
	c.Width = float64(len(days))
	c.YTicks = yTicks(axisMax, step)
	c.XTicks = xTicks(days)
	c.Bars = make([]chartBar, len(days))
	const gap = 0.22
	for i, v := range values {
		h := 0.0
		if axisMax > 0 && v > 0 {
			h = max(float64(v)/float64(axisMax)*plotHeight, 1.2)
		}
		c.Bars[i] = chartBar{
			X: float64(i) + gap/2, Y: plotHeight - h, W: 1 - gap, H: h,
			Day: days[i], Value: v, Last: i == len(days)-1,
		}
	}
	return c
}

// yScale picks a step of 1, 2, or 5 times a power of ten, aiming for four to
// six labels from zero up to a round ceiling.
func yScale(peak int64) (max, step int64) {
	if peak <= 0 {
		return 1, 1
	}
	step = niceStep(float64(peak) / 4)
	if step < 1 {
		step = 1
	}
	for {
		max = ((peak + step - 1) / step) * step
		if int(max/step)+1 >= 4 || step == 1 {
			return max, step
		}
		next := shrinkStep(step)
		if next >= step {
			return max, step
		}
		step = next
	}
}

func yTicks(max, step int64) []yTick {
	if step < 1 {
		step = 1
	}
	n := int(max/step) + 1
	ticks := make([]yTick, 0, n)
	for i := n - 1; i >= 0; i-- {
		v := int64(i) * step
		top := (1 - float64(v)/float64(max)) * 100
		edge := ""
		switch i {
		case n - 1:
			edge = "tick-start"
		case 0:
			edge = "tick-end"
		}
		ticks = append(ticks, yTick{Value: v, Y: top / 100 * plotHeight, Top: top, Edge: edge})
	}
	return ticks
}

func niceStep(raw float64) int64 {
	if raw <= 1 {
		return 1
	}
	exp := math.Floor(math.Log10(raw))
	pow := math.Pow(10, exp)
	frac := raw / pow
	nice := 10.0
	switch {
	case frac <= 1:
		nice = 1
	case frac <= 2:
		nice = 2
	case frac <= 5:
		nice = 5
	}
	return int64(math.Round(nice * pow))
}

func shrinkStep(step int64) int64 {
	if step <= 1 {
		return 1
	}
	pow := int64(math.Round(math.Pow(10, math.Floor(math.Log10(float64(step))))))
	if pow < 1 {
		return 1
	}
	switch step / pow {
	case 5:
		return 2 * pow
	case 2:
		return pow
	default:
		if next := pow / 2; next >= 1 {
			return next
		}
		return 1
	}
}

func xTicks(days []string) []xTick {
	idx := xIndex(len(days))
	out := make([]xTick, 0, len(idx))
	for i, at := range idx {
		label := days[at]
		edge := ""
		if t, err := time.Parse("2006-01-02", days[at]); err == nil {
			label = t.Format("Jan 2")
		}
		left := 0.0
		if len(days) > 0 {
			left = (float64(at) + 0.5) / float64(len(days)) * 100
		}
		switch i {
		case 0:
			edge, left = "tick-start", 0
		case len(idx) - 1:
			edge, left = "tick-end", 100
		}
		out = append(out, xTick{Label: label, Left: left, Edge: edge})
	}
	return out
}

func xIndex(n int) []int {
	if n <= 0 {
		return nil
	}
	want := 6
	switch {
	case n <= 7:
		want = n
	case n <= 14:
		want = 7
	}
	if want > n {
		want = n
	}
	out := make([]int, 0, want)
	prev := -1
	for i := 0; i < want; i++ {
		at := 0
		if i == want-1 {
			at = n - 1
		} else if want > 1 {
			at = int(math.Round(float64(i) * float64(n-1) / float64(want-1)))
		}
		if at == prev {
			continue
		}
		out = append(out, at)
		prev = at
	}
	return out
}

func (d *T) downloadLeaders(ctx context.Context, page *statsPage, listed map[string]listingHead, from, to string) error {
	byApp, err := d.analytics.QueryDownloads(ctx, astore.DownloadFilter{From: from, To: to, GroupBy: []string{"app_id"}})
	if err != nil {
		return err
	}
	for _, row := range byApp {
		name := row.AppID
		if head, ok := listed[row.AppID]; ok && head.name != "" {
			name = head.name
		}
		page.TopApps = append(page.TopApps, topApp{ID: row.AppID, Name: name, Downloads: row.Count})
	}
	slices.SortFunc(page.TopApps, func(a, b topApp) int {
		if c := cmp.Compare(b.Downloads, a.Downloads); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	page.TopApps = page.TopApps[:min(len(page.TopApps), topAppCount)]
	// Share is relative to the leader, for the bar behind each row.
	if len(page.TopApps) > 0 && page.TopApps[0].Downloads > 0 {
		lead := float64(page.TopApps[0].Downloads)
		for i := range page.TopApps {
			page.TopApps[i].Share = float64(page.TopApps[i].Downloads) / lead * 100
		}
	}

	byType, err := d.analytics.QueryDownloads(ctx, astore.DownloadFilter{From: from, To: to, GroupBy: []string{"type"}})
	if err != nil {
		return err
	}
	for _, row := range byType {
		switch row.Type {
		case astore.Install:
			page.Installs += row.Count
		case astore.Update:
			page.Updates += row.Count
		}
	}
	return nil
}

// dayRange returns every day from `from` to `to` inclusive in ascending order.
func dayRange(from, to string) []string {
	start, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	var days []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}
