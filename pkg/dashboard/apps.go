package dashboard

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/nwt"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/steroid/artifacts"
)

// EnrichFunc enriches one app and returns the enricher report.
type EnrichFunc func(ctx context.Context, appID string) (string, error)

// appsPerPage is how many rows the list renders before "Load more".
const appsPerPage = 50

const (
	filterAll      = ""
	filterMissing  = "missing"
	filterUnlisted = "unlisted"
)

type appRow struct {
	ID        string
	Name      string
	Summary   string
	Updated   time.Time
	Listed    bool
	Enriched  bool
	C1        bool
	Downloads int
}

type appsPage struct {
	Query        string
	Filter       string
	Rows         []appRow
	Total        int
	Listed       int
	Enriched     int
	Missing      int
	Unlisted     int
	Next         int
	NextHref     string
	NextCount    int
	Remaining    int
	HasArtifacts bool
	HasDownloads bool
	Now          time.Time
}

func (d *T) appsPage(w http.ResponseWriter, r *http.Request, _ nwt.Token) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := r.URL.Query().Get("filter")
	if filter != filterMissing && filter != filterUnlisted {
		filter = filterAll
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)

	listed, err := d.listings(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	enriched, hasArtifacts, err := d.artifactIDs(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	c1, err := d.c1Active(ctx, listed)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	all := appRows(listed, enriched)
	for i := range all {
		all[i].C1 = c1[all[i].ID]
	}
	page := appsPage{Query: q, Filter: filter, HasArtifacts: hasArtifacts, Now: time.Now()}
	for _, row := range all {
		switch {
		case !row.Listed:
			page.Unlisted++
		case row.Enriched:
			page.Listed++
			page.Enriched++
		default:
			page.Listed++
			page.Missing++
		}
	}
	rows := filterRows(all, q, filter)
	page.Total = len(rows)
	end := min(offset+appsPerPage, len(rows))
	if offset < end {
		page.Rows = rows[offset:end]
	}
	if end < len(rows) {
		page.Next = end
		page.NextHref = appsHref(q, filter, end)
		page.Remaining = len(rows) - end
		page.NextCount = min(page.Remaining, appsPerPage)
	}

	if d.analytics != nil && len(page.Rows) > 0 {
		ids := make([]string, len(page.Rows))
		for i, row := range page.Rows {
			ids[i] = row.ID
		}
		counts, err := d.analytics.QueryDownloadsByAppIDs(ctx, ids, daysAgo(30), "")
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		for i := range page.Rows {
			page.Rows[i].Downloads = counts[page.Rows[i].ID]
		}
		page.HasDownloads = true
	}

	if r.URL.Query().Get("part") == "rows" {
		d.render(w, "app-rows", page)
		return
	}
	d.render(w, "apps", page)
}

// appRows merges listings with apps that only have artifacts, newest first.
func appRows(listed map[string]listingHead, enriched map[string]bool) []appRow {
	rows := make([]appRow, 0, len(listed)+len(enriched))
	for id, head := range listed {
		summary, _ := events.Find(head.tags, "summary")
		rows = append(rows, appRow{
			ID: id, Name: head.name, Summary: summary,
			Updated: time.Unix(head.at, 0), Listed: true, Enriched: enriched[id],
		})
	}
	for id := range enriched {
		if _, ok := listed[id]; !ok {
			rows = append(rows, appRow{ID: id, Enriched: true})
		}
	}
	slices.SortFunc(rows, func(a, b appRow) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows
}

// c1Slot is the publisher an active identity proof authorizes for one certificate.
type c1Slot struct {
	owner    string
	delegate string
}

// c1Active is the listed apps whose signing certificate has an active identity
// proof for the listing publisher, as owner or delegate.
func (d *T) c1Active(ctx context.Context, listed map[string]listingHead) (map[string]bool, error) {
	if d.relay.Store == nil || len(listed) == 0 {
		return map[string]bool{}, nil
	}
	certs, err := d.listingCerts(ctx, listed)
	if err != nil {
		return nil, err
	}
	winners, err := d.c1Winners(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	return activeApps(listed, certs, winners), nil
}

// listingCerts is the newest apk_certificate_hash each listing publisher
// signed for that app.
func (d *T) listingCerts(ctx context.Context, listed map[string]listingHead) (map[string]string, error) {
	want := make(map[string]string, len(listed))
	for id, head := range listed {
		want[id] = strings.ToLower(head.pubkey)
	}
	rows, err := d.relay.DB.QueryContext(ctx, `SELECT pubkey, created_at, tags FROM events WHERE kind = ?`, events.KindAsset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pick struct {
		at   int64
		cert string
	}
	best := map[string]pick{}
	for rows.Next() {
		var pubkey string
		var at int64
		var tags nostr.Tags
		if err := rows.Scan(&pubkey, &at, &tags); err != nil {
			return nil, err
		}
		id, ok := events.Find(tags, "i")
		hash, has := events.Find(tags, "apk_certificate_hash")
		if !ok || !has || hash == "" || want[id] != strings.ToLower(pubkey) {
			continue
		}
		if cur, ok := best[id]; ok && at <= cur.at {
			continue
		}
		best[id] = pick{at: at, cert: strings.ToLower(hash)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	certs := make(map[string]string, len(best))
	for id, pick := range best {
		certs[id] = pick.cert
	}
	return certs, nil
}

// c1Winners is the current active proof for each certificate: newest event
// per pubkey, then the newest pubkey that is still active.
func (d *T) c1Winners(ctx context.Context, at time.Time) (map[string]c1Slot, error) {
	rows, err := d.relay.DB.QueryContext(ctx, `SELECT id, pubkey, created_at, tags FROM events WHERE kind = ?`, events.KindIdentityProof)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proofs []nostr.Event
	for rows.Next() {
		var ev nostr.Event
		var created int64
		if err := rows.Scan(&ev.ID, &ev.PubKey, &created, &ev.Tags); err != nil {
			return nil, err
		}
		ev.Kind = events.KindIdentityProof
		ev.CreatedAt = nostr.Timestamp(created)
		proofs = append(proofs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return c1Slots(proofs, at), nil
}

func c1Slots(proofs []nostr.Event, at time.Time) map[string]c1Slot {
	byCert := map[string]map[string]nostr.Event{}
	for _, ev := range proofs {
		cert, ok := events.Find(ev.Tags, "d")
		if !ok || cert == "" {
			continue
		}
		cert = strings.ToLower(cert)
		if byCert[cert] == nil {
			byCert[cert] = map[string]nostr.Event{}
		}
		prev, exists := byCert[cert][ev.PubKey]
		if exists && !newerProof(ev, prev) {
			continue
		}
		byCert[cert][ev.PubKey] = ev
	}
	out := make(map[string]c1Slot, len(byCert))
	for cert, current := range byCert {
		var best *nostr.Event
		for _, ev := range current {
			if !events.ProofActive(ev, at) {
				continue
			}
			if best == nil || newerProof(ev, *best) {
				chosen := ev
				best = &chosen
			}
		}
		if best == nil {
			continue
		}
		delegate, _ := events.Find(best.Tags, "delegation")
		out[cert] = c1Slot{owner: best.PubKey, delegate: delegate}
	}
	return out
}

func newerProof(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}

func activeApps(listed map[string]listingHead, certs map[string]string, winners map[string]c1Slot) map[string]bool {
	out := map[string]bool{}
	for id, head := range listed {
		slot, ok := winners[certs[id]]
		if certs[id] == "" || !ok {
			continue
		}
		if strings.EqualFold(head.pubkey, slot.owner) || (slot.delegate != "" && strings.EqualFold(head.pubkey, slot.delegate)) {
			out[id] = true
		}
	}
	return out
}

// filterRows keeps rows matching the filter and the query. A query ranks
// exact matches, then prefixes, then substrings; ties stay newest first.
func filterRows(rows []appRow, q, filter string) []appRow {
	q = strings.ToLower(q)
	type ranked struct {
		row  appRow
		rank int
	}
	var hits []ranked
	for _, row := range rows {
		switch filter {
		case filterMissing:
			if !row.Listed || row.Enriched {
				continue
			}
		case filterUnlisted:
			if row.Listed {
				continue
			}
		}
		rank := matchRank(q, strings.ToLower(row.ID), strings.ToLower(row.Name))
		if rank < 0 {
			continue
		}
		hits = append(hits, ranked{row, rank})
	}
	slices.SortStableFunc(hits, func(a, b ranked) int { return cmp.Compare(a.rank, b.rank) })
	out := make([]appRow, len(hits))
	for i, h := range hits {
		out[i] = h.row
	}
	return out
}

func matchRank(q, id, name string) int {
	switch {
	case q == "":
		return 0
	case id == q || name == q:
		return 0
	case strings.HasPrefix(id, q) || strings.HasPrefix(name, q):
		return 1
	case strings.Contains(id, q) || strings.Contains(name, q):
		return 2
	default:
		return -1
	}
}

func appsHref(q, filter string, offset int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if filter != "" {
		v.Set("filter", filter)
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
		v.Set("part", "rows")
	}
	if len(v) == 0 {
		return "/apps"
	}
	return "/apps?" + v.Encode()
}

// appIcon serves the enricher's icon, else redirects to the listing icon.
// Icons are public catalog data, so this route needs no token and an img tag can use it.
func (d *T) appIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validAppID(id) {
		fail(w, http.StatusBadRequest, errors.New("invalid app id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, closeDB, err := d.artifactDB(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if closeDB != nil {
		defer closeDB()
	}
	if db != nil {
		app, err := db.Load(ctx, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if len(app.Icon) > 0 {
			w.Header().Set("Content-Type", "image/webp")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(app.Icon)
			return
		}
	}
	head, ok, err := d.listing(ctx, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	icon, _ := events.Find(head.tags, "icon")
	if !ok || !webURL(icon) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.Redirect(w, r, icon, http.StatusFound)
}

type listingHead struct {
	id      string
	name    string
	pubkey  string
	content string
	tags    nostr.Tags
	at      int64
	count   int
}

// listings is the newest listing per app id.
func (d *T) listings(ctx context.Context) (map[string]listingHead, error) {
	if d.relay.Store == nil {
		return map[string]listingHead{}, nil
	}
	rows, err := d.relay.DB.QueryContext(ctx, `SELECT id, created_at, pubkey, content, tags FROM events WHERE kind = ?`, events.KindApp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	best := map[string]listingHead{}
	for rows.Next() {
		var row listingHead
		if err := rows.Scan(&row.id, &row.at, &row.pubkey, &row.content, &row.tags); err != nil {
			return nil, err
		}
		id, ok := events.Find(row.tags, "d")
		if !ok || id == "" {
			continue
		}
		row.name, _ = events.Find(row.tags, "name")
		keepNewest(best, id, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

// listing is the newest listing for one app, with how many publishers list it.
func (d *T) listing(ctx context.Context, id string) (listingHead, bool, error) {
	if d.relay.Store == nil {
		return listingHead{}, false, nil
	}
	evs, err := d.relay.Query(ctx, nostr.Filter{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": {id}}, Limit: 20})
	if err != nil {
		return listingHead{}, false, err
	}
	best := map[string]listingHead{}
	for _, ev := range evs {
		row := listingHead{id: ev.ID, pubkey: ev.PubKey, content: ev.Content, tags: ev.Tags, at: int64(ev.CreatedAt)}
		row.name, _ = events.Find(ev.Tags, "name")
		keepNewest(best, id, row)
	}
	head, ok := best[id]
	head.count = len(evs)
	return head, ok, nil
}

func keepNewest(best map[string]listingHead, id string, row listingHead) {
	cur, ok := best[id]
	if ok && row.at <= cur.at {
		return
	}
	best[id] = row
}

// artifactIDs is the set of enriched apps. ok is false when artifacts.db is missing.
func (d *T) artifactIDs(ctx context.Context) (map[string]bool, bool, error) {
	db, closeDB, err := d.artifactDB(ctx)
	if err != nil || db == nil {
		return map[string]bool{}, false, err
	}
	if closeDB != nil {
		defer closeDB()
	}
	ids, err := db.IDs(ctx)
	if err != nil {
		return nil, false, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, true, nil
}

// artifactDB is the process connection, or a read of a database created after startup.
// The closer is set only for the connection opened here.
func (d *T) artifactDB(ctx context.Context) (*artifacts.DB, func(), error) {
	if d.artifacts != nil {
		return d.artifacts, nil, nil
	}
	db, err := artifacts.OpenRead(ctx, artifacts.Path(d.dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return db, func() { db.Close() }, nil
}

func validAppID(id string) bool {
	return id != "" && id == filepath.Base(id) && !strings.Contains(id, "..")
}

func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func daysAgo(n int) string {
	return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02")
}
