package dashboard

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	bhash "github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/nwt"
	astore "github.com/zapstore/relay/pkg/analytics/store"
	blobs "github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
)

const (
	// maxReleases is how many releases the app page lists.
	maxReleases   = 30
	enrichTimeout = 15 * time.Minute
)

type appPage struct {
	ID    string
	Title string
	Found bool

	Listing   *listingView
	Latest    *releaseView
	Older     []releaseView
	Publisher *publisherView

	Artifacts    *artifactsView
	ArtifactsDB  bool
	Security     *securityView
	Downloads    int
	HasDownloads bool

	IsAdmin   bool
	CanBan    bool
	CanEnrich bool
	Jobs      []jobView
	Now       time.Time

	Cert  string  // signing certificate of the latest release
	Proof *c1View // best proof for Cert
}

type listingView struct {
	EventID    string
	Summary    string
	Content    string
	URL        string
	Repository string
	License    string
	Platforms  []string
	Tags       []string
	Images     []string
	Updated    time.Time
	Others     int
}

type releaseView struct {
	EventID   string
	Version   string
	Channel   string
	Notes     string
	Created   time.Time
	Downloads int
	Assets    []assetView
	Missing   int
}

type assetView struct {
	Hash        string
	Filename    string
	Variant     string
	URL         string
	MIME        string
	Size        int64
	Platforms   []string
	VersionCode string
	MinSDK      string
	TargetSDK   string
	Commit      string
	Cert        string
	Ancestors   []string
	CertBanned  bool
	Stored      string
	BanReason   string
	Banned      bool
}

type publisherView struct {
	Pubkey  string
	Name    string
	NIP05   string
	Website string
	Banned  bool
}

// c1View is one NIP-C1 proof for this publisher: a certificate they own, or
// one whose owner delegated publishing to them.
type c1View struct {
	Cert    string
	Status  string // Active, Expired, or Revoked
	Role    string // Owner or Delegate
	Other   string // delegate pubkey, or the owner's when Role is Delegate
	Reason  string
	Expiry  time.Time
	Matches bool    // certificate signs the latest release
	Apps    []c1App // apps whose assets use this certificate
}

// c1App is an app a certificate has signed.
type c1App struct {
	ID   string
	Name string
}

type artifactsView struct {
	About   string
	Queries []string
	Vector  bool
	Icon    bool
	APK     string
	Current bool
	Stale   bool
}

func (d *T) appPage(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	id := r.PathValue("id")
	if !validAppID(id) {
		fail(w, http.StatusBadRequest, errors.New("invalid app id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	page, err := d.loadApp(ctx, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	admin := d.auth.IsAdmin(token)
	page.IsAdmin = admin
	page.CanEnrich = admin && d.enrich != nil
	page.CanBan = admin && !d.readOnly && d.ban != nil
	page.Jobs = withStop(d.jobs.list(func(kind, target string) bool {
		return kind == jobEnrich && target == id
	}), admin)
	d.render(w, "app", page)
}

func (d *T) loadApp(ctx context.Context, id string) (appPage, error) {
	page := appPage{ID: id, Title: id, Now: time.Now()}

	head, listed, err := d.listing(ctx, id)
	if err != nil {
		return page, err
	}
	if listed {
		page.Found = true
		page.Listing = listingFrom(head)
		page.Title = shownTitle(head.name, id)
		pub, err := d.publisher(ctx, head.pubkey)
		if err != nil {
			return page, err
		}
		page.Publisher = &pub
	}

	releases, err := d.releases(ctx, id, head.pubkey)
	if err != nil {
		return page, err
	}
	if len(releases) > 0 {
		page.Found = true
		if err := d.checkAssets(ctx, &releases[0]); err != nil {
			return page, err
		}
		page.Latest = &releases[0]
		page.Older = releases[1:]
	}
	if page.Publisher != nil {
		page.Cert = latestCert(page.Latest)
		proofs, err := d.c1Proofs(ctx, page.Publisher.Pubkey, page.Cert)
		if err != nil {
			return page, err
		}
		matched, _ := assignProofs([]pubApp{{Cert: page.Cert}}, proofs)
		page.Cert = matched[0].Cert
		page.Proof = matched[0].Proof
	}
	if d.analytics != nil {
		page.HasDownloads = true
		if err := d.downloads(ctx, &page, releases); err != nil {
			return page, err
		}
	}

	db, closeDB, err := d.artifactDB(ctx)
	if err != nil {
		return page, err
	}
	if closeDB != nil {
		defer closeDB()
	}
	if db != nil {
		page.ArtifactsDB = true
		app, err := db.Load(ctx, id)
		if err != nil {
			return page, err
		}
		if app.Present {
			page.Found = true
			a := &artifactsView{
				About:   strings.TrimSpace(app.About),
				Queries: lines(app.Queries),
				Vector:  len(app.Vector) > 0,
				Icon:    len(app.Icon) > 0,
				APK:     app.Memo.APK,
			}
			if page.Latest != nil && a.APK != "" {
				a.Current = slices.ContainsFunc(page.Latest.Assets, func(as assetView) bool { return as.Hash == a.APK })
				a.Stale = !a.Current
			}
			page.Artifacts = a
			page.Security = parseSecurity(app.Security)
		}
	}
	return page, nil
}

func listingFrom(head listingHead) *listingView {
	v := &listingView{EventID: head.id, Updated: time.Unix(head.at, 0), Others: max(head.count-1, 0)}
	event := nostr.Event{Kind: events.KindApp, PubKey: head.pubkey, Content: head.content, Tags: head.tags}
	app, err := events.ParseApp(&event)
	if err != nil {
		v.Content = strings.TrimSpace(head.content)
		return v
	}
	v.Summary = app.Summary
	v.Content = strings.TrimSpace(app.Content)
	v.Platforms = app.Platforms
	v.Tags = app.Tags
	v.License = app.License
	if webURL(app.URL) {
		v.URL = app.URL
	}
	if webURL(app.Repository) {
		v.Repository = app.Repository
	}
	for _, img := range app.Images {
		if webURL(img) {
			v.Images = append(v.Images, img)
		}
	}
	return v
}

// releases are the app's newest releases with their assets, newest first.
// With a listing, only its publisher's releases count, as in the catalog.
func (d *T) releases(ctx context.Context, id, pubkey string) ([]releaseView, error) {
	if d.relay.Store == nil {
		return nil, nil
	}
	filter := nostr.Filter{Kinds: []int{events.KindRelease}, Tags: nostr.TagMap{"i": {id}}, Limit: maxReleases}
	if pubkey != "" {
		filter.Authors = []string{pubkey}
	}
	evs, err := d.relay.Query(ctx, filter)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(evs, func(a, b nostr.Event) int { return int(b.CreatedAt - a.CreatedAt) })

	var all []string
	out := make([]releaseView, 0, len(evs))
	refs := make([][]string, 0, len(evs))
	for _, ev := range evs {
		rel, err := events.ParseRelease(&ev)
		if err != nil {
			continue
		}
		out = append(out, releaseView{
			EventID: ev.ID, Version: rel.Version, Channel: rel.Channel,
			Notes: strings.TrimSpace(rel.Content), Created: ev.CreatedAt.Time(),
		})
		refs = append(refs, rel.AssetIDs)
		all = append(all, rel.AssetIDs...)
	}
	if len(all) == 0 {
		return out, nil
	}

	assetEvs, err := d.relay.Query(ctx, nostr.Filter{Kinds: []int{events.KindAsset}, IDs: all, Limit: len(all)})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]assetView, len(assetEvs))
	for _, ev := range assetEvs {
		a, err := events.ParseAsset(&ev)
		if err != nil {
			continue
		}
		byID[ev.ID] = assetFrom(a)
	}
	for i, ids := range refs {
		for _, id := range ids {
			if a, ok := byID[id]; ok {
				out[i].Assets = append(out[i].Assets, a)
			} else {
				out[i].Missing++
			}
		}
	}
	return out, nil
}

func assetFrom(a events.Asset) assetView {
	v := assetView{
		Hash: a.Hash, Filename: a.Filename, Variant: a.Variant, MIME: a.MimeType,
		Platforms: a.Platforms, VersionCode: a.VersionCode,
		MinSDK: a.MinPlatformVersion, TargetSDK: a.TargetPlatformVersion,
		Commit: a.Commit, Ancestors: a.APKCertificateAncestors,
	}
	if webURL(a.URL) {
		v.URL = a.URL
	}
	v.Size, _ = strconv.ParseInt(a.Size, 10, 64)
	if len(a.APKCertificateHashes) > 0 {
		v.Cert = a.APKCertificateHashes[0]
	}
	return v
}

// checkAssets fills storage and ban state for one release.
func (d *T) checkAssets(ctx context.Context, rel *releaseView) error {
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if d.blossom != nil {
			state, err := d.blobState(ctx, a.Hash)
			if err != nil {
				return err
			}
			a.Stored = state
		}
		if d.pol == nil {
			continue
		}
		reason, banned, err := d.pol.HashReason(ctx, a.Hash)
		if err != nil {
			return err
		}
		a.Banned, a.BanReason = banned, reason
		if a.Cert != "" {
			if pub, err := policy.Normalize(policy.KindCert, a.Cert); err == nil {
				if a.CertBanned, err = d.pol.Blacklisted(ctx, pub); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// blobState is the blossom state of a blob, or "missing".
func (d *T) blobState(ctx context.Context, hex string) (string, error) {
	h, err := bhash.ParseHash(hex)
	if err != nil {
		return "missing", nil
	}
	meta, err := d.blossom.Query(ctx, h)
	if errors.Is(err, blobs.ErrBlobNotFound) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	return string(meta.State), nil
}

func (d *T) downloads(ctx context.Context, page *appPage, releases []releaseView) error {
	rows, err := d.analytics.QueryDownloads(ctx, astore.DownloadFilter{
		AppID: page.ID, From: daysAgo(30), GroupBy: []string{"app_version"},
	})
	if err != nil {
		return err
	}
	byVersion := make(map[string]int, len(rows))
	for _, row := range rows {
		byVersion[row.AppVersion] += row.Count
		page.Downloads += row.Count
	}
	if page.Latest != nil {
		page.Latest.Downloads = byVersion[page.Latest.Version]
	}
	for i := range page.Older {
		page.Older[i].Downloads = byVersion[page.Older[i].Version]
	}
	return nil
}

// latestCert is the signing certificate of the latest release's first asset that has one.
func latestCert(rel *releaseView) string {
	if rel == nil {
		return ""
	}
	for _, asset := range rel.Assets {
		if asset.Cert != "" {
			return strings.ToLower(asset.Cert)
		}
	}
	return ""
}

// c1Proofs loads identity proofs this publisher signed. When appCert is set,
// it also keeps a proof for that certificate that names them as delegate.
// When it is empty, it keeps recent proofs that delegate to them.
func (d *T) c1Proofs(ctx context.Context, pubkey, appCert string) ([]c1View, error) {
	if d.relay.Store == nil {
		return nil, nil
	}
	authored, err := d.relay.Query(ctx, nostr.Filter{
		Kinds: []int{events.KindIdentityProof}, Authors: []string{pubkey}, Limit: 20,
	})
	if err != nil {
		return nil, err
	}
	eventsByID := map[string]nostr.Event{}
	for _, ev := range authored {
		eventsByID[ev.ID] = ev
	}
	if appCert != "" {
		linked, err := d.relay.Query(ctx, nostr.Filter{
			Kinds: []int{events.KindIdentityProof},
			Tags:  nostr.TagMap{"d": {appCert}},
			Limit: 20,
		})
		if err != nil {
			return nil, err
		}
		for _, ev := range linked {
			delegate, _ := events.Find(ev.Tags, "delegation")
			if ev.PubKey == pubkey || strings.EqualFold(delegate, pubkey) {
				eventsByID[ev.ID] = ev
			}
		}
	} else {
		recent, err := d.relay.Query(ctx, nostr.Filter{
			Kinds: []int{events.KindIdentityProof}, Limit: 200,
		})
		if err != nil {
			return nil, err
		}
		for _, ev := range recent {
			delegate, _ := events.Find(ev.Tags, "delegation")
			if strings.EqualFold(delegate, pubkey) {
				eventsByID[ev.ID] = ev
			}
		}
	}
	list := make([]nostr.Event, 0, len(eventsByID))
	for _, ev := range eventsByID {
		list = append(list, ev)
	}
	return c1Views(pubkey, appCert, list, time.Now()), nil
}

// c1Views describes the proofs that belong to pubkey. appCert marks the one
// that signs the latest release.
func c1Views(pubkey, appCert string, list []nostr.Event, now time.Time) []c1View {
	views := make([]c1View, 0, len(list))
	for _, ev := range list {
		proof, err := events.ParseIdentityProof(&ev)
		if err != nil || proof.CertHash == "" {
			continue
		}
		delegate, _ := events.Find(ev.Tags, "delegation")
		if ev.PubKey != pubkey && !strings.EqualFold(delegate, pubkey) {
			continue
		}
		view := c1View{
			Cert:    strings.ToLower(proof.CertHash),
			Matches: appCert != "" && strings.EqualFold(proof.CertHash, appCert),
		}
		switch {
		case proof.Revoked:
			view.Status = "Revoked"
			view.Reason = proof.Reason
		case !events.ProofActive(ev, now):
			view.Status = "Expired"
		default:
			view.Status = "Active"
		}
		if proof.Expiry > 0 {
			view.Expiry = time.Unix(proof.Expiry, 0)
		}
		if ev.PubKey == pubkey {
			view.Role = "Owner"
			view.Other = delegate
		} else {
			view.Role = "Delegate"
			view.Other = ev.PubKey
		}
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b c1View) int {
		if a.Matches != b.Matches {
			if a.Matches {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(c1Rank(a.Status), c1Rank(b.Status)); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Expiry.Unix(), a.Expiry.Unix()); c != 0 {
			return c
		}
		return cmp.Compare(a.Cert, b.Cert)
	})
	return views
}

func c1Rank(status string) int {
	switch status {
	case "Active":
		return 0
	case "Expired":
		return 1
	default:
		return 2
	}
}

// publisher reads the profile and ban state of a listing's signer.
func (d *T) publisher(ctx context.Context, pubkey string) (publisherView, error) {
	v := publisherView{Pubkey: strings.ToLower(pubkey)}
	if d.pol != nil {
		if pub, err := policy.Normalize(policy.KindNostr, pubkey); err == nil {
			banned, err := d.pol.Blacklisted(ctx, pub)
			if err != nil {
				return v, err
			}
			v.Banned = banned
		}
	}
	if d.relay.Store == nil {
		return v, nil
	}
	evs, err := d.relay.Query(ctx, nostr.Filter{Kinds: []int{events.KindProfile}, Authors: []string{v.Pubkey}, Limit: 1})
	if err != nil || len(evs) == 0 {
		return v, err
	}
	var meta struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		NIP05       string `json:"nip05"`
		Website     string `json:"website"`
	}
	if json.Unmarshal([]byte(evs[0].Content), &meta) != nil {
		return v, nil
	}
	v.Name = strings.TrimSpace(cmp.Or(meta.DisplayName, meta.Name))
	v.NIP05 = meta.NIP05
	if webURL(meta.Website) {
		v.Website = meta.Website
	}
	return v, nil
}

func shownTitle(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func (d *T) enrichApp(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	id := r.PathValue("id")
	if !validAppID(id) {
		fail(w, http.StatusBadRequest, errors.New("invalid app id"))
		return
	}
	if d.enrich == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("enrich is not available on this relay"))
		return
	}
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	_, err := d.jobs.start(jobEnrich, id, note(r), token.Signer, enrichTimeout, func(ctx context.Context) (string, error) {
		out, err := d.enrich(ctx, id)
		if err == nil && strings.TrimSpace(out) == "" {
			out = "Nothing to enrich: the app has no listing"
		}
		return out, err
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	d.render(w, "jobs", withStop(d.jobs.list(func(kind, target string) bool {
		return kind == jobEnrich && target == id
	}), true))
}
