package dashboard

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/nwt"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
)

type publisherRow struct {
	Pubkey string
	Name   string
	Label  string
	Count  int
	Banned bool
	Newest time.Time
}

type pubApp struct {
	ID      string
	Name    string
	Updated time.Time
	Cert    string  // current signing certificate, if any asset has one
	Proof   *c1View // best proof for Cert
}

type publishersPage struct {
	Query string
	Rows  []publisherRow
	Total int
	Now   time.Time
}

type publisherPage struct {
	Pubkey    string
	Publisher publisherView
	Apps      []pubApp
	Extra     []c1View // proofs that are not an app's current signer
	CanBan    bool
	Now       time.Time
}

func (d *T) publishersPage(w http.ResponseWriter, r *http.Request, _ nwt.Token) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, total, err := d.publisherRows(ctx, q)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	d.render(w, "publishers", publishersPage{Query: q, Rows: rows, Total: total, Now: time.Now()})
}

func (d *T) publisherPage(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	pubkey := strings.ToLower(hexKey(strings.TrimSpace(r.PathValue("id"))))
	if !validPubkey(pubkey) {
		fail(w, http.StatusBadRequest, errors.New("invalid publisher"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pub, err := d.publisher(ctx, pubkey)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	proofs, err := d.c1Proofs(ctx, pubkey, "")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	apps, err := d.publisherApps(ctx, pubkey)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := d.signingCerts(ctx, pubkey, apps); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	apps, extra := assignProofs(apps, proofs)
	if err := d.nameCertApps(ctx, extra); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	admin := d.auth.IsAdmin(token)
	d.render(w, "publisher", publisherPage{
		Pubkey:    pubkey,
		Publisher: pub,
		Apps:      apps,
		Extra:     extra,
		CanBan:    admin && !d.readOnly && d.ban != nil,
		Now:       time.Now(),
	})
}

// publisherRows is one row per listing signer. total counts every signer,
// before the search filter.
func (d *T) publisherRows(ctx context.Context, q string) ([]publisherRow, int, error) {
	listed, err := d.listings(ctx)
	if err != nil {
		return nil, 0, err
	}
	type acc struct {
		names  []string
		ids    []string
		newest int64
	}
	by := map[string]*acc{}
	var keys []string
	for id, head := range listed {
		pubkey := strings.ToLower(head.pubkey)
		if !validPubkey(pubkey) {
			continue
		}
		row, ok := by[pubkey]
		if !ok {
			row = &acc{}
			by[pubkey] = row
			keys = append(keys, pubkey)
		}
		name := head.name
		if name == "" {
			name = id
		}
		row.names = append(row.names, name)
		row.ids = append(row.ids, id)
		if head.at > row.newest {
			row.newest = head.at
		}
	}
	names, err := d.profileNames(ctx, keys)
	if err != nil {
		return nil, 0, err
	}
	banned, err := d.bannedNostr(ctx)
	if err != nil {
		return nil, 0, err
	}
	needle := strings.ToLower(q)
	rows := make([]publisherRow, 0, len(by))
	for pubkey, row := range by {
		slices.SortFunc(row.names, func(a, b string) int {
			return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
		})
		label := strings.Join(row.names, ", ")
		if len(row.names) > 3 {
			label = strings.Join(row.names[:3], ", ")
		}
		item := publisherRow{
			Pubkey: pubkey,
			Name:   names[pubkey],
			Label:  label,
			Count:  len(row.ids),
			Banned: banned[pubkey],
			Newest: time.Unix(row.newest, 0).UTC(),
		}
		if needle != "" && !strings.Contains(publisherHay(item, row.ids, row.names), needle) {
			continue
		}
		rows = append(rows, item)
	}
	slices.SortFunc(rows, func(a, b publisherRow) int {
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.ToLower(cmp.Or(a.Name, a.Pubkey)), strings.ToLower(cmp.Or(b.Name, b.Pubkey))); c != 0 {
			return c
		}
		return cmp.Compare(a.Pubkey, b.Pubkey)
	})
	return rows, len(by), nil
}

func publisherHay(row publisherRow, ids, names []string) string {
	parts := make([]string, 0, 4+len(ids)+len(names))
	parts = append(parts, row.Name, row.Pubkey, npub(row.Pubkey))
	parts = append(parts, ids...)
	parts = append(parts, names...)
	return strings.ToLower(strings.Join(parts, " "))
}

func (d *T) publisherApps(ctx context.Context, pubkey string) ([]pubApp, error) {
	listed, err := d.listings(ctx)
	if err != nil {
		return nil, err
	}
	var apps []pubApp
	for id, head := range listed {
		if !strings.EqualFold(head.pubkey, pubkey) {
			continue
		}
		apps = append(apps, pubApp{
			ID:      id,
			Name:    cmp.Or(head.name, id),
			Updated: time.Unix(head.at, 0).UTC(),
		})
	}
	slices.SortFunc(apps, func(a, b pubApp) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return apps, nil
}

// signingCerts sets each app's current certificate: the newest asset this
// publisher signed for that app that carries an apk_certificate_hash.
func (d *T) signingCerts(ctx context.Context, pubkey string, apps []pubApp) error {
	if d.relay.Store == nil || len(apps) == 0 {
		return nil
	}
	want := make(map[string]int, len(apps))
	for i := range apps {
		want[apps[i].ID] = i
	}
	rows, err := d.relay.DB.QueryContext(ctx, `
		SELECT created_at, tags FROM events WHERE kind = ? AND pubkey = ?
	`, events.KindAsset, pubkey)
	if err != nil {
		return err
	}
	defer rows.Close()
	newest := map[string]int64{}
	for rows.Next() {
		var at int64
		var tags nostr.Tags
		if err := rows.Scan(&at, &tags); err != nil {
			return err
		}
		id, ok := events.Find(tags, "i")
		if !ok {
			continue
		}
		i, listed := want[id]
		hash, ok := events.Find(tags, "apk_certificate_hash")
		if !listed || !ok || hash == "" || at < newest[id] {
			continue
		}
		newest[id] = at
		apps[i].Cert = strings.ToLower(hash)
	}
	return rows.Err()
}

// assignProofs attaches the best proof for each app's current certificate.
// Proofs for any other certificate are returned, one per certificate.
func assignProofs(apps []pubApp, proofs []c1View) ([]pubApp, []c1View) {
	best := make(map[string]c1View, len(proofs))
	for _, proof := range proofs {
		cert := strings.ToLower(proof.Cert)
		proof.Cert = cert
		cur, ok := best[cert]
		if !ok || proofBetter(proof, cur) {
			best[cert] = proof
		}
	}
	used := map[string]bool{}
	for i := range apps {
		cert := strings.ToLower(apps[i].Cert)
		apps[i].Cert = cert
		proof, ok := best[cert]
		if cert == "" || !ok {
			continue
		}
		apps[i].Proof = &proof
		used[cert] = true
	}
	var extra []c1View
	for cert, proof := range best {
		if used[cert] {
			continue
		}
		extra = append(extra, proof)
	}
	slices.SortFunc(extra, func(a, b c1View) int {
		if c := cmp.Compare(c1Rank(a.Status), c1Rank(b.Status)); c != 0 {
			return c
		}
		return cmp.Compare(a.Cert, b.Cert)
	})
	return apps, extra
}

func proofBetter(a, b c1View) bool {
	if c1Rank(a.Status) != c1Rank(b.Status) {
		return c1Rank(a.Status) < c1Rank(b.Status)
	}
	return a.Expiry.After(b.Expiry)
}

// nameCertApps fills the apps whose assets use each leftover certificate.
func (d *T) nameCertApps(ctx context.Context, proofs []c1View) error {
	if d.relay.Store == nil || len(proofs) == 0 {
		return nil
	}
	args := make([]any, 0, len(proofs)+1)
	args = append(args, events.KindAsset)
	marks := make([]string, len(proofs))
	for i, proof := range proofs {
		marks[i] = "?"
		args = append(args, strings.ToLower(proof.Cert))
	}
	rows, err := d.relay.DB.QueryContext(ctx, `
		SELECT lower(t.value), e.tags
		FROM tags t
		JOIN events e ON e.id = t.event_id
		WHERE e.kind = ? AND t.key = 'apk_certificate_hash' AND lower(t.value) IN (`+strings.Join(marks, ",")+`)
	`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]map[string]bool{}
	for rows.Next() {
		var cert string
		var tags nostr.Tags
		if err := rows.Scan(&cert, &tags); err != nil {
			return err
		}
		id, ok := events.Find(tags, "i")
		if !ok || id == "" {
			continue
		}
		if seen[cert] == nil {
			seen[cert] = map[string]bool{}
		}
		seen[cert][id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	listed, err := d.listings(ctx)
	if err != nil {
		return err
	}
	for i := range proofs {
		ids := seen[strings.ToLower(proofs[i].Cert)]
		apps := make([]c1App, 0, len(ids))
		for id := range ids {
			name := id
			if head, ok := listed[id]; ok && head.name != "" {
				name = head.name
			}
			apps = append(apps, c1App{ID: id, Name: name})
		}
		slices.SortFunc(apps, func(a, b c1App) int {
			if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
		proofs[i].Apps = apps
	}
	return nil
}

// profileNames is the display name of each pubkey, from the newest profile.
func (d *T) profileNames(ctx context.Context, pubkeys []string) (map[string]string, error) {
	names := map[string]string{}
	if d.relay.Store == nil || len(pubkeys) == 0 {
		return names, nil
	}
	want := make(map[string]bool, len(pubkeys))
	for _, pubkey := range pubkeys {
		want[pubkey] = true
	}
	rows, err := d.relay.DB.QueryContext(ctx, `SELECT pubkey, created_at, content FROM events WHERE kind = ?`, events.KindProfile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := map[string]int64{}
	for rows.Next() {
		var pubkey, content string
		var created int64
		if err := rows.Scan(&pubkey, &created, &content); err != nil {
			return nil, err
		}
		pubkey = strings.ToLower(pubkey)
		if !want[pubkey] || created <= at[pubkey] {
			continue
		}
		at[pubkey] = created
		names[pubkey] = profileName(content)
	}
	return names, rows.Err()
}

func profileName(content string) string {
	var meta struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if json.Unmarshal([]byte(content), &meta) != nil {
		return ""
	}
	return strings.TrimSpace(cmp.Or(meta.DisplayName, meta.Name))
}

func (d *T) bannedNostr(ctx context.Context) (map[string]bool, error) {
	set := map[string]bool{}
	if d.pol == nil {
		return set, nil
	}
	rows, err := d.pol.Publishers(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Kind == policy.KindNostr {
			set[strings.ToLower(row.ID)] = true
		}
	}
	return set, nil
}

func validPubkey(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
