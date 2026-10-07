package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/zapstore/relay/pkg/policy"
)

type blacklistRow struct {
	Kind    string
	ID      string
	Reason  string
	Created string
}

type hashRow struct {
	Hash   string
	Reason string
}

type blacklistPageData struct {
	IsAdmin    bool
	Error      string
	Publishers []blacklistRow
	Hashes     []hashRow
}

func (d *T) blacklistPage(w http.ResponseWriter, r *http.Request) {
	token, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	d.renderBlacklist(w, r, d.auth.IsAdmin(token), "")
}

func (d *T) addBan(w http.ResponseWriter, r *http.Request) {
	token, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	if !d.auth.IsAdmin(token) {
		http.Error(w, "forbidden: admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		d.renderBlacklist(w, r, true, "invalid form")
		return
	}
	if d.ban == nil {
		d.renderBlacklist(w, r, true, "ban is not available")
		return
	}
	pub, err := policy.Normalize(r.FormValue("kind"), r.FormValue("id"))
	if err != nil {
		d.renderBlacklist(w, r, true, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := d.ban(ctx, pub, r.FormValue("reason")); err != nil {
		d.renderBlacklist(w, r, true, err.Error())
		return
	}
	d.renderBlacklist(w, r, true, "")
}

func (d *T) removeBan(w http.ResponseWriter, r *http.Request) {
	token, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	if !d.auth.IsAdmin(token) {
		http.Error(w, "forbidden: admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		d.renderBlacklist(w, r, true, "invalid form")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.RemovePublisher(ctx, r.FormValue("kind"), r.FormValue("id")); err != nil {
		d.renderBlacklist(w, r, true, err.Error())
		return
	}
	d.renderBlacklist(w, r, true, "")
}

func (d *T) removeHash(w http.ResponseWriter, r *http.Request) {
	token, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	if !d.auth.IsAdmin(token) {
		http.Error(w, "forbidden: admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		d.renderBlacklist(w, r, true, "invalid form")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.pol.RemoveHash(ctx, r.FormValue("hash")); err != nil {
		d.renderBlacklist(w, r, true, err.Error())
		return
	}
	d.renderBlacklist(w, r, true, "")
}

func (d *T) renderBlacklist(w http.ResponseWriter, r *http.Request, isAdmin bool, errMsg string) {
	if d.pol == nil {
		http.Error(w, "blacklist is not available", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	publishers, err := d.pol.Publishers(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hashes, err := d.pol.Hashes(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := blacklistPageData{
		IsAdmin:    isAdmin,
		Error:      errMsg,
		Publishers: make([]blacklistRow, 0, len(publishers)),
		Hashes:     make([]hashRow, 0, len(hashes)),
	}
	for _, row := range publishers {
		created := ""
		if row.CreatedAt > 0 {
			created = time.Unix(row.CreatedAt, 0).UTC().Format("2006-01-02")
		}
		data.Publishers = append(data.Publishers, blacklistRow{
			Kind: row.Kind, ID: row.ID, Reason: row.Reason, Created: created,
		})
	}
	for _, row := range hashes {
		data.Hashes = append(data.Hashes, hashRow{Hash: row.Hash, Reason: row.Reason})
	}
	if err := d.template.ExecuteTemplate(w, "blacklist", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
