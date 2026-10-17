package dashboard

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

func newTestDashboard(t *testing.T, cfg Config, db relay.DB) *T {
	t.Helper()
	limiter := rate.NewLimiter(rate.Config{InitialTokens: 1000, MaxTokens: 1000, TokensPerInterval: 1000, Interval: time.Second})
	d, err := New(cfg, limiter, nil, nil, db, nil, nil, nil, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newRelayDB(t *testing.T) relay.DB {
	t.Helper()
	db, err := relay.NewDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func render(t *testing.T, d *T, name string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := d.template.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

func wantAll(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("body:\n%s", body)
	}
}

// signedToken returns an Authorization header for a fresh key, and that key's pubkey.
func signedToken(t *testing.T, audience string) (string, string) {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	ev := nostr.Event{
		Kind:      27519,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"aud", audience}, {"exp", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}},
	}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return "Nostr " + base64.RawURLEncoding.EncodeToString(raw), pk
}

func TestKeepNewest(t *testing.T) {
	best := map[string]listingHead{}
	keepNewest(best, "com.app", listingHead{name: "Old", at: 1})
	keepNewest(best, "com.app", listingHead{name: "New", at: 2})
	keepNewest(best, "com.app", listingHead{name: "Tied", at: 2})
	keepNewest(best, "other.app", listingHead{name: "Other", at: 1})
	if best["com.app"].name != "New" || best["other.app"].name != "Other" {
		t.Fatalf("%+v", best)
	}
	keepNewest(best, "com.app", listingHead{name: "Older", at: 1})
	if best["com.app"].name != "New" {
		t.Fatalf("older replaced newest: %+v", best)
	}
}

func TestListings(t *testing.T) {
	db := newRelayDB(t)
	older := &nostr.Event{
		ID: "old", PubKey: "aa", CreatedAt: 1, Kind: events.KindApp, Sig: "s",
		Tags: nostr.Tags{{"d", "org.signal.app"}, {"name", "Old"}},
	}
	newer := &nostr.Event{
		ID: "new", PubKey: "bb", CreatedAt: 2, Kind: events.KindApp, Sig: "s",
		Content: "Private messenger",
		Tags:    nostr.Tags{{"d", "org.signal.app"}, {"name", "Signal"}, {"summary", "Messages"}, {"f", "android-arm64-v8a"}, {"url", "https://signal.org"}, {"repository", "javascript:alert(1)"}},
	}
	for _, event := range []*nostr.Event{older, newer} {
		if _, err := db.Save(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	d := &T{relay: db}
	got, err := d.listings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := got["org.signal.app"]; row.name != "Signal" || row.content != "Private messenger" || row.id != "new" {
		t.Fatalf("listings: %+v", row)
	}

	head, ok, err := d.listing(t.Context(), "org.signal.app")
	if err != nil || !ok {
		t.Fatalf("listing: ok %v err %v", ok, err)
	}
	if head.name != "Signal" || head.count != 2 {
		t.Fatalf("listing: %+v", head)
	}
	v := listingFrom(head)
	if v.Summary != "Messages" || v.Content != "Private messenger" || v.Others != 1 || v.URL != "https://signal.org" {
		t.Fatalf("listingFrom: %+v", v)
	}
	if v.Repository != "" {
		t.Fatalf("unsafe repository URL kept: %q", v.Repository)
	}
}

func TestFilterRows(t *testing.T) {
	rows := appRows(map[string]listingHead{
		"org.signal.app":         {name: "Signal", at: 3},
		"org.telegram.messenger": {name: "Telegram", at: 2},
		"app.signalish":          {name: "Not Signal", at: 4},
	}, map[string]bool{"org.signal.app": true, "com.orphan": true})

	if rows[0].ID != "app.signalish" || rows[len(rows)-1].ID != "com.orphan" {
		t.Fatalf("not newest first: %+v", rows)
	}

	tests := []struct {
		q, filter string
		want      []string
	}{
		{"", "", []string{"app.signalish", "org.signal.app", "org.telegram.messenger", "com.orphan"}},
		{"signal", "", []string{"org.signal.app", "app.signalish"}},
		{"org.", "", []string{"org.signal.app", "org.telegram.messenger"}},
		{"", filterMissing, []string{"app.signalish", "org.telegram.messenger"}},
		{"", filterUnlisted, []string{"com.orphan"}},
		{"nothing", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.q+"/"+tt.filter, func(t *testing.T) {
			got := filterRows(rows, tt.q, tt.filter)
			var ids []string
			for _, row := range got {
				ids = append(ids, row.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Errorf("filterRows(%q, %q) = %v, want %v", tt.q, tt.filter, ids, tt.want)
			}
		})
	}
}

func TestC1Active(t *testing.T) {
	pub := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	cert := strings.Repeat("c", 64)
	now := time.Unix(1_000, 0)
	active := nostr.Event{
		ID: strings.Repeat("1", 64), PubKey: pub, CreatedAt: 50, Kind: events.KindIdentityProof,
		Tags: nostr.Tags{{"d", cert}, {"expiry", "5000"}, {"delegation", other}},
	}
	older := active
	older.ID = strings.Repeat("9", 64)
	older.CreatedAt = 10
	expired := active
	expired.ID = strings.Repeat("2", 64)
	expired.PubKey = other
	expired.CreatedAt = 80
	expired.Tags = nostr.Tags{{"d", cert}, {"expiry", "10"}}
	slots := c1Slots([]nostr.Event{older, expired, active}, now)
	if slots[cert].owner != pub || slots[cert].delegate != other {
		t.Fatalf("winner %+v", slots[cert])
	}
	listed := map[string]listingHead{
		"org.signal.app": {pubkey: pub},
		"com.delegated":  {pubkey: other},
		"com.stranger":   {pubkey: strings.Repeat("e", 64)},
		"com.nocert":     {pubkey: pub},
	}
	got := activeApps(listed, map[string]string{
		"org.signal.app": cert, "com.delegated": cert, "com.stranger": cert,
	}, slots)
	if !got["org.signal.app"] || !got["com.delegated"] || got["com.stranger"] || got["com.nocert"] {
		t.Fatalf("active %+v", got)
	}

	db := newRelayDB(t)
	save := func(ev *nostr.Event) {
		t.Helper()
		ev.Sig = "s"
		if _, err := db.Save(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	save(&nostr.Event{ID: "asset-old", PubKey: pub, CreatedAt: 10, Kind: events.KindAsset, Tags: nostr.Tags{{"i", "org.signal.app"}, {"apk_certificate_hash", strings.Repeat("d", 64)}}})
	save(&nostr.Event{ID: "asset-new", PubKey: pub, CreatedAt: 20, Kind: events.KindAsset, Tags: nostr.Tags{{"i", "org.signal.app"}, {"apk_certificate_hash", cert}}})
	stored := active
	stored.Tags = nostr.Tags{{"d", cert}, {"expiry", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)}, {"delegation", other}}
	save(&stored)
	d := newTestDashboard(t, Config{}, db)
	live, err := d.c1Active(t.Context(), map[string]listingHead{"org.signal.app": {pubkey: pub}})
	if err != nil {
		t.Fatal(err)
	}
	if !live["org.signal.app"] {
		t.Fatal("stored proof did not mark the app active")
	}
}

func TestAppsHref(t *testing.T) {
	tests := []struct {
		q, filter string
		offset    int
		want      string
	}{
		{"", "", 0, "/apps"},
		{"sig nal", "", 0, "/apps?q=sig+nal"},
		{"x", filterMissing, 50, "/apps?filter=missing&offset=50&part=rows&q=x"},
	}
	for _, tt := range tests {
		if got := appsHref(tt.q, tt.filter, tt.offset); got != tt.want {
			t.Errorf("appsHref(%q, %q, %d) = %q, want %q", tt.q, tt.filter, tt.offset, got, tt.want)
		}
	}
}

func TestParseSecurity(t *testing.T) {
	if parseSecurity("  ") != nil {
		t.Fatal("empty artifact is not nil")
	}
	if v := parseSecurity("not json"); v == nil || v.Raw != "not json" {
		t.Fatalf("raw fallback: %+v", v)
	}
	v := parseSecurity(`{
		"facts": {"google_services": false, "tracking": true, "source_available": true},
		"notes": {"tracking": "Firebase Analytics", "CAMERA": "QR scanning"},
		"permissions": ["CAMERA", "android.permission.INTERNET", "READ_CALL_LOG", "FOO_BAR_BAZ"],
		"notice": "Contains a tracker", "body": "Looks fine otherwise.",
		"keywords": ["messenger"]
	}`)
	var facts []string
	for _, f := range v.Facts {
		facts = append(facts, f.Label+":"+f.Tone)
	}
	want := "No Google services:ok,Tracks users:bad,Source available:ok"
	if strings.Join(facts, ",") != want {
		t.Errorf("facts = %v, want %s", facts, want)
	}
	if v.Facts[1].Note != "Firebase Analytics" {
		t.Errorf("tracking note = %q", v.Facts[1].Note)
	}
	if strings.Join(v.Unreported, ",") != "Ads,Offline use,Encryption" {
		t.Errorf("unreported = %v", v.Unreported)
	}
	if len(v.Sensitive) != 2 || v.Sensitive[0].Label != "Camera" || v.Sensitive[0].Note != "QR scanning" || v.Sensitive[1].Label != "Read call log" {
		t.Errorf("sensitive = %+v", v.Sensitive)
	}
	if len(v.Other) != 2 || v.Other[0].Label != "Internet" || v.Other[1].Label != "Foo bar baz" {
		t.Errorf("other = %+v", v.Other)
	}
	if v.Notice != "Contains a tracker" || v.Body != "Looks fine otherwise." || len(v.Keywords) != 1 {
		t.Errorf("prose = %+v", v)
	}
}

func TestAppTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	pub := "bb" + strings.Repeat("0", 62)
	page := appPage{
		ID: "org.signal.app", Title: "Signal", Found: true,
		Listing: &listingView{
			EventID: "ev1", Summary: "Messages", Content: "Long description",
			URL: "https://signal.org", Repository: "https://github.com/signalapp/Signal-Android",
			License: "AGPL-3.0", Platforms: []string{"android-arm64-v8a"}, Images: []string{"https://img/1.png"},
			Updated: time.Now().Add(-2 * time.Hour),
		},
		Latest: &releaseView{
			EventID: "rel1", Version: "7.1.0", Notes: "Bug fixes", Created: time.Now(), Downloads: 12,
			Assets: []assetView{{Hash: strings.Repeat("a", 64), Filename: "signal.apk", Size: 52 << 20, MinSDK: "26", Cert: strings.Repeat("c", 64), Stored: "missing", Banned: true, BanReason: "malware"}},
		},
		Older:     []releaseView{{Version: "7.0.0", Created: time.Now().Add(-48 * time.Hour)}},
		Publisher: &publisherView{Pubkey: pub, Name: "Signal Foundation", Banned: true},
		Cert:      strings.Repeat("c", 64),
		Proof: &c1View{
			Cert: strings.Repeat("c", 64), Status: "Active", Role: "Owner", Other: strings.Repeat("d", 64),
			Expiry: time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC),
		},
		Artifacts:    &artifactsView{About: "A private messenger.", Queries: []string{"private chat"}, Vector: true, APK: strings.Repeat("d", 64), Stale: true},
		ArtifactsDB:  true,
		Security:     parseSecurity(`{"facts":{"source_available":true},"permissions":["CAMERA"]}`),
		HasDownloads: true, Downloads: 99,
		IsAdmin: true, CanEnrich: true, CanBan: true,
		Jobs: []jobView{{ID: 3, Kind: jobEnrich, Target: "org.signal.app", Running: true, CanStop: true, Note: "new APK", Started: time.Now()}},
		Now:  time.Now(),
	}
	body := render(t, d, "app", page)
	wantAll(t, body,
		"<title>Signal · Zapstore relay</title>",
		`src="/apps/org.signal.app/icon"`,
		`<span class="ver">7.1.0</span>`,
		"52.0 MB",
		"Messages",
		"A private messenger.",
		"Description from the listing",
		`<details class="fold">`,
		"Source available",
		"Camera",
		"Not stored",
		"File banned",
		"malware",
		"The publisher of this listing is banned",
		`class="app-author"`,
		`href="/publishers/`+pub+`"`,
		"Signal Foundation",
		"Active", "Delegates to", "2027-03-04",
		"The artifacts describe an older APK",
		`hx-post="/apps/org.signal.app/enrich"`,
		`hx-get="/jobs/3?poll=1"`,
		"Note: new APK",
		"private chat",
		`target="_blank"`,
		"7.0.0",
	)
	if art, rel := strings.Index(body, `id="artifacts"`), strings.Index(body, `id="releases"`); art < 0 || art > rel {
		t.Error("artifacts are not in the main column before releases")
	}
	if strings.Contains(body, `<details class="fold" open`) {
		t.Error("about text is expanded by default")
	}
	if strings.Contains(body, "Review ban") {
		t.Error("ban offered for a publisher already banned")
	}
	head := body[:strings.Index(body, `class="facts"`)]
	if !strings.Contains(head, `class="app-author"`) || strings.Contains(head, `class="app-id"`) {
		t.Error("author should sit under the icon, without the package id")
	}
	page.Publisher.Name = ""
	bare := render(t, d, "app", page)
	bareHead := bare[:strings.Index(bare, `class="facts"`)]
	if !strings.Contains(bareHead, "npub1") {
		t.Error("author with no profile name does not show an npub")
	}
	page.Publisher.Banned = false
	shown := render(t, d, "app", page)
	if strings.Contains(shown, "Review ban") || !strings.Contains(shown, `/publishers/`+pub) {
		t.Error("app page should link the publisher and leave the ban on their page")
	}
	if strings.Contains(body, `name="note"`) {
		t.Error("enrich form asks for a note")
	}
	if strings.Contains(body, `id="app-live" class="layout" hx-`) {
		t.Error("app refresh target wraps the page, so links inside it inherit hx-select")
	}
	if strings.Contains(body, `"source_available"`) {
		t.Error("security JSON rendered raw")
	}

	viewer := render(t, d, "app", appPage{ID: "x.y", Title: "X", Found: true, Now: time.Now()})
	if strings.Contains(viewer, "hx-post") {
		t.Errorf("viewer sees actions\n%s", viewer)
	}
	missing := render(t, d, "app", appPage{ID: "x.y", Title: "x.y"})
	wantAll(t, missing, "This relay knows nothing about x.y")
}

func TestC1Views(t *testing.T) {
	owner := strings.Repeat("a", 64)
	delegate := strings.Repeat("b", 64)
	cert := strings.Repeat("c", 64)
	other := strings.Repeat("d", 64)
	now := time.Unix(1_700_000_000, 0)
	active := nostr.Event{
		ID: strings.Repeat("1", 64), PubKey: owner, Kind: events.KindIdentityProof,
		CreatedAt: nostr.Timestamp(now.Unix() - 10),
		Tags: nostr.Tags{
			{"d", cert},
			{"expiry", strconv.FormatInt(now.Unix()+1000, 10)},
			{"delegation", delegate},
		},
	}
	got := c1Views(delegate, cert, []nostr.Event{active}, now)
	if len(got) != 1 || got[0].Role != "Delegate" || got[0].Status != "Active" || !got[0].Matches || got[0].Other != owner {
		t.Fatalf("delegate view %+v", got)
	}
	own := c1Views(owner, cert, []nostr.Event{active}, now)
	if len(own) != 1 || own[0].Role != "Owner" || own[0].Other != delegate {
		t.Fatalf("owner view %+v", own)
	}

	revoked := active
	revoked.ID = strings.Repeat("2", 64)
	revoked.Tags = nostr.Tags{{"d", other}, {"revoked", "key leaked"}}
	expired := active
	expired.ID = strings.Repeat("3", 64)
	expired.Tags = nostr.Tags{{"d", cert}, {"expiry", "10"}}
	views := c1Views(owner, cert, []nostr.Event{revoked, expired, active}, now)
	if len(views) != 3 || views[0].Status != "Active" || views[1].Status != "Expired" || views[2].Status != "Revoked" || views[2].Reason != "key leaked" {
		t.Fatalf("order %+v", views)
	}
	if stranger := c1Views(strings.Repeat("e", 64), cert, []nostr.Event{active}, now); len(stranger) != 0 {
		t.Fatalf("unrelated proof kept %+v", stranger)
	}
}

func TestAppsTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	page := appsPage{
		Query: "sig", Listed: 2, Enriched: 1, Missing: 1, HasArtifacts: true, HasDownloads: true,
		Rows:  []appRow{{ID: "org.signal.app", Name: "Signal", Summary: "Messages", Listed: true, Enriched: true, C1: true, Downloads: 1234, Updated: time.Now()}},
		Total: 60, Next: 50, NextHref: "/apps?offset=50&part=rows&q=sig", NextCount: 10, Remaining: 10,
		Now: time.Now(),
	}
	body := render(t, d, "apps", page)
	wantAll(t, body, `href="/apps/org.signal.app"`, "Signal", "1,234", `class="tag sm">Enriched`, ">C1<", `name="q" value="sig"`,
		`hx-push-url="true"`, "Show 10 more of 10", `hx-trigger="every 60s [canRefresh(this)], job-done from:body"`)
	section := body[strings.Index(body, `id="apps-live"`):]
	poll := strings.Index(section, `class="live-poll"`)
	row := strings.Index(section, `class="row"`)
	if poll < 0 || row < 0 || row < poll+strings.Index(section[poll:], "</span>") {
		t.Fatal("app links inherit the list refresh selector, so a click renders nothing")
	}

	rows := render(t, d, "app-rows", page)
	if strings.Contains(rows, "<title>") {
		t.Error("row fragment carries a page title")
	}
}

func TestRoutes(t *testing.T) {
	db := newRelayDB(t)
	if _, err := db.Save(t.Context(), &nostr.Event{
		ID: "e1", PubKey: strings.Repeat("a", 64), CreatedAt: 5, Kind: events.KindApp, Sig: "s",
		Tags: nostr.Tags{{"d", "org.signal.app"}, {"name", "Signal"}, {"f", "android-arm64-v8a"}, {"icon", "https://cdn.example/icon.png"}},
	}); err != nil {
		t.Fatal(err)
	}
	auth, pk := signedToken(t, "dash.test")
	d := newTestDashboard(t, Config{Hostname: "dash.test", ViewerPubkeys: []string{pk}}, db)
	srv := httptest.NewServer(gzipped(d.routes()))
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	get := func(path string, hx bool, header string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res, string(b)
	}

	if res, _ := get("/", false, ""); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/apps" {
		t.Errorf("/ = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, path := range []string{"/apps", "/apps/org.signal.app", "/publishers", "/stats", "/bundles", "/moderation", "/apps?q=sig"} {
		res, body := get(path, false, "")
		if res.StatusCode != http.StatusOK || !strings.Contains(body, `hx-trigger="load"`) || !strings.Contains(body, `hx-get="`+strings.ReplaceAll(path, "&", "&amp;")+`"`) {
			t.Errorf("shell %s = %d\n%s", path, res.StatusCode, body)
		}
	}
	if res, _ := get("/apps", true, ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("fragment without token = %d", res.StatusCode)
	}

	res, body := get("/apps", true, auth)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Signal") || strings.Contains(body, "<html") {
		t.Errorf("apps fragment = %d\n%s", res.StatusCode, body)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("fragment Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	pub := strings.Repeat("a", 64)
	res, body = get("/apps/org.signal.app", true, auth)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>Signal</h1>") || !strings.Contains(body, `/publishers/`+pub) {
		t.Errorf("app fragment = %d\n%s", res.StatusCode, body)
	}
	res, body = get("/publishers", true, auth)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `/publishers/`+pub) || !strings.Contains(body, "Signal") || strings.Contains(body, "<html") {
		t.Errorf("publishers fragment = %d\n%s", res.StatusCode, body)
	}
	res, body = get("/publishers/"+pub, true, auth)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `href="/apps/org.signal.app"`) || !strings.Contains(body, "No certificate") {
		t.Errorf("publisher fragment = %d\n%s", res.StatusCode, body)
	}
	if res, _ := get("/publishers/not-a-key", true, auth); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad publisher = %d", res.StatusCode)
	}
	if res, _ := get("/apps/..%2Fetc", true, auth); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id = %d", res.StatusCode)
	}
	if res, _ := get("/moderation", true, auth); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("moderation without policy = %d", res.StatusCode)
	}
	if res, _ := get("/stats", true, auth); res.StatusCode != http.StatusOK {
		t.Errorf("stats = %d", res.StatusCode)
	}
	if res, _ := get("/bundles", true, auth); res.StatusCode != http.StatusOK {
		t.Errorf("bundles = %d", res.StatusCode)
	}

	res, _ = get("/apps/org.signal.app/icon", false, "")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://cdn.example/icon.png" {
		t.Errorf("icon fallback = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := get("/apps/com.none/icon", false, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing icon = %d", res.StatusCode)
	}

	res, _ = get("/static/dashboard.css?v="+d.version, false, "")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("static = %d %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}

	// POST actions need an admin.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/apps/org.signal.app/enrich", nil)
	req.Header.Set("Authorization", auth)
	postRes, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusForbidden {
		t.Errorf("viewer enrich = %d", postRes.StatusCode)
	}
}

func TestGzip(t *testing.T) {
	d := newTestDashboard(t, Config{Hostname: "h"}, relay.DB{})
	srv := httptest.NewServer(gzipped(d.routes()))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/login", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("login not gzipped: %v", res.Header)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil || !strings.Contains(string(b), "Sign in") {
		t.Fatalf("body %q err %v", b, err)
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/static/dashboard.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", `"`+d.version+`"`)
	res2, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	body, _ := io.ReadAll(res2.Body)
	if res2.StatusCode != http.StatusNotModified || len(body) != 0 || res2.Header.Get("Content-Encoding") != "" {
		t.Fatalf("304: status %d encoding %q body %d bytes", res2.StatusCode, res2.Header.Get("Content-Encoding"), len(body))
	}
}

func TestViewFuncs(t *testing.T) {
	tests := []struct{ got, want string }{
		{num(1234567), "1,234,567"},
		{num(int64(-1200)), "-1,200"},
		{num(12), "12"},
		{short(strings.Repeat("a", 64)), "aaaaaaaa…aaaaaa"},
		{short("abc"), "abc"},
		{formatSize(52 << 20), "52.0 MB"},
		{formatSize(0), ""},
		{letter("signal"), "S"},
		{letter(""), "?"},
		{ago(time.Now().Add(-5 * time.Minute)), "5m ago"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
	if got := npub(strings.Repeat("0", 64)); !strings.HasPrefix(got, "npub1") {
		t.Errorf("npub = %q", got)
	}
}
