package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/pippellia-btc/nwt"
	"github.com/zapstore/relay/pkg/policy"
)

type banRow struct {
	Kind    string
	ID      string
	Reason  string
	Created time.Time
}

type hashRow struct {
	Hash   string
	Reason string
}

// banForm is the ban form's values, prefilled from a link or kept after an error.
type banForm struct {
	Kind   string
	ID     string
	Reason string
	App    string
}

type allowRow struct {
	ID      string
	Created time.Time
}

type allowForm struct {
	ID string
}

type moderationPage struct {
	IsAdmin    bool
	ReadOnly   bool
	CanBan     bool
	CanAllow   bool
	Notice     string
	Error      string
	Form       banForm
	AllowForm  allowForm
	Publishers []banRow
	Hashes     []hashRow
	Allowed    []allowRow
	Now        time.Time
}

func (d *T) moderationPage(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	q := r.URL.Query()
	form := banForm{Kind: q.Get("kind"), ID: q.Get("id"), Reason: q.Get("reason"), App: q.Get("app")}
	if form.Reason == "" && form.App != "" {
		form.Reason = "Publisher of " + form.App
	}
	d.renderModeration(w, r, d.auth.IsAdmin(token), form, allowForm{}, "", "")
}

func (d *T) addBan(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	form := banForm{Kind: r.FormValue("kind"), ID: strings.TrimSpace(r.FormValue("id")), Reason: strings.TrimSpace(r.FormValue("reason"))}
	if msg := d.writeBlocked(); msg != "" {
		d.renderModeration(w, r, true, form, allowForm{}, "", msg)
		return
	}
	if d.ban == nil {
		d.renderModeration(w, r, true, form, allowForm{}, "", "Banning is not available on this relay")
		return
	}
	id := form.ID
	if form.Kind == policy.KindNostr {
		id = hexKey(id)
	}
	pub, err := policy.Normalize(form.Kind, id)
	if err != nil {
		d.renderModeration(w, r, true, form, allowForm{}, "", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := d.ban(ctx, pub, form.Reason); err != nil {
		d.renderModeration(w, r, true, form, allowForm{}, "", err.Error())
		return
	}
	slog.Info("dashboard: banned", "kind", pub.Kind, "id", pub.ID, "by", token.Signer)
	w.Header().Set("HX-Push-Url", "/moderation")
	d.renderModeration(w, r, true, banForm{}, allowForm{}, fmt.Sprintf("Banned %s %s. Their listings are removed from the catalog.", kindLabel(pub.Kind), shortID(pub)), "")
}

func (d *T) removeBan(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	if msg := d.writeBlocked(); msg != "" {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", msg)
		return
	}
	kind, id := r.FormValue("kind"), r.FormValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.RemovePublisher(ctx, kind, id); err != nil {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", err.Error())
		return
	}
	slog.Info("dashboard: unbanned", "kind", kind, "id", id, "by", token.Signer)
	d.renderModeration(w, r, true, banForm{}, allowForm{}, fmt.Sprintf("Removed %s %s from the blacklist. Listings already deleted stay deleted.", kindLabel(kind), short(id)), "")
}

func (d *T) removeHash(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	if msg := d.writeBlocked(); msg != "" {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", msg)
		return
	}
	hash := r.FormValue("hash")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.RemoveHash(ctx, hash); err != nil {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", err.Error())
		return
	}
	slog.Info("dashboard: file allowed", "hash", hash, "by", token.Signer)
	d.renderModeration(w, r, true, banForm{}, allowForm{}, "Allowed file "+short(hash)+" again.", "")
}

func (d *T) addAllow(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	form := allowForm{ID: strings.TrimSpace(r.FormValue("id"))}
	if msg := d.writeBlocked(); msg != "" {
		d.renderModeration(w, r, true, banForm{}, form, "", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.Allow(ctx, hexKey(form.ID)); err != nil {
		d.renderModeration(w, r, true, banForm{}, form, "", err.Error())
		return
	}
	slog.Info("dashboard: whitelisted", "id", form.ID, "by", token.Signer)
	w.Header().Set("HX-Push-Url", "/moderation")
	d.renderModeration(w, r, true, banForm{}, allowForm{}, "Whitelisted "+short(npub(hexKey(form.ID)))+". They can publish without a web of trust score.", "")
}

func (d *T) removeAllow(w http.ResponseWriter, r *http.Request, token nwt.Token) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid form"))
		return
	}
	if msg := d.writeBlocked(); msg != "" {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", msg)
		return
	}
	id := r.FormValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.Disallow(ctx, id); err != nil {
		d.renderModeration(w, r, true, banForm{}, allowForm{}, "", err.Error())
		return
	}
	slog.Info("dashboard: removed from whitelist", "id", id, "by", token.Signer)
	d.renderModeration(w, r, true, banForm{}, allowForm{}, "Removed "+short(npub(id))+" from the whitelist.", "")
}

// writeBlocked explains why a moderation change cannot run, or is empty.
func (d *T) writeBlocked() string {
	switch {
	case d.readOnly:
		return "This relay is read-only, so moderation changes are off"
	case d.pol == nil:
		return "Moderation is not available on this relay"
	}
	return ""
}

func (d *T) renderModeration(w http.ResponseWriter, r *http.Request, isAdmin bool, form banForm, allow allowForm, notice, errMsg string) {
	if d.pol == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("moderation is not available on this relay"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	publishers, err := d.pol.Publishers(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	hashes, err := d.pol.Hashes(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	allowed, err := d.pol.AllowedKeys(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if form.Kind == "" {
		form.Kind = policy.KindNostr
	}
	page := moderationPage{
		IsAdmin:    isAdmin,
		ReadOnly:   d.readOnly,
		CanBan:     isAdmin && !d.readOnly && d.ban != nil,
		CanAllow:   isAdmin && !d.readOnly,
		Notice:     notice,
		Error:      errMsg,
		Form:       form,
		AllowForm:  allow,
		Publishers: make([]banRow, 0, len(publishers)),
		Hashes:     make([]hashRow, 0, len(hashes)),
		Allowed:    make([]allowRow, 0, len(allowed)),
		Now:        time.Now(),
	}
	for _, row := range publishers {
		ban := banRow{Kind: row.Kind, ID: row.ID, Reason: row.Reason}
		if row.CreatedAt > 0 {
			ban.Created = time.Unix(row.CreatedAt, 0)
		}
		page.Publishers = append(page.Publishers, ban)
	}
	slices.Reverse(page.Publishers)
	for _, row := range hashes {
		page.Hashes = append(page.Hashes, hashRow{Hash: row.Hash, Reason: row.Reason})
	}
	for _, row := range allowed {
		item := allowRow{ID: row.ID}
		if row.CreatedAt > 0 {
			item.Created = time.Unix(row.CreatedAt, 0)
		}
		page.Allowed = append(page.Allowed, item)
	}
	d.render(w, "moderation", page)
}

func kindLabel(kind string) string {
	switch kind {
	case policy.KindNostr:
		return "publisher"
	case policy.KindForge:
		return "forge account"
	case policy.KindCert:
		return "certificate"
	}
	return kind
}

// hexKey decodes an npub. Anything else is returned for Normalize to judge.
func hexKey(id string) string {
	if !strings.HasPrefix(id, "npub1") {
		return id
	}
	prefix, v, err := nip19.Decode(id)
	if err != nil || prefix != "npub" {
		return id
	}
	if s, ok := v.(string); ok {
		return s
	}
	return id
}

func shortID(pub policy.Publisher) string {
	if pub.Kind == policy.KindNostr {
		return short(npub(pub.ID))
	}
	return short(pub.ID)
}
