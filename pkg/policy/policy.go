// Package policy stores the publisher blacklist, banned file hashes,
// ban notices, and the suggestion queue in the relay database.
package policy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/repourl"
)

//go:embed schema.sql
var schema string

const (
	KindNostr = "nostr"
	KindForge = "forge"
	KindCert  = "cert"
)

var (
	ErrPublisher = errors.New("invalid publisher")
	ErrReason    = errors.New("reason is empty")
	ErrSuggest   = errors.New("suggestion needs a repository or a release source")
)

// Publisher is a Nostr key, a forge account (host/user), or an APK certificate.
type Publisher struct {
	Kind string
	ID   string
}

// Suggestion is one row in the queue.
type Suggestion struct {
	ID              string `json:"id"`
	Repository      string `json:"repository"`
	ReleaseSource   string `json:"release_source"`
	CertificateHash string `json:"certificate_hash"`
}

// Notice is the operator text for one removed app. The snapshot copies it.
type Notice struct {
	AppID  string
	Reason string
}

// Store is the policy tables in the catalog database.
type Store struct {
	db *sql.DB
}

// Open creates the policy tables in an existing catalog database.
func Open(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("policy schema: %w", err)
	}
	if err := migrateAllowed(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrateAllowed copies the old paid-publisher table when a database still has it.
func migrateAllowed(db *sql.DB) error {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'allowed'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("policy schema: %w", err)
	}
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO paid_access (kind, id, reason, created_at)
		SELECT kind, id, reason, created_at FROM allowed
	`); err != nil {
		return fmt.Errorf("migrate allowed: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE allowed`); err != nil {
		return fmt.Errorf("migrate allowed: %w", err)
	}
	return nil
}

// Normalize checks a publisher id.
func Normalize(kind, id string) (Publisher, error) {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	switch kind {
	case KindNostr, KindCert:
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 32 {
			return Publisher{}, fmt.Errorf("%w: %s id must be 64 hex characters", ErrPublisher, kind)
		}
		return Publisher{Kind: kind, ID: hex.EncodeToString(raw)}, nil
	case KindForge:
		account, err := forgeAccount(id)
		if err != nil {
			return Publisher{}, err
		}
		return Publisher{Kind: KindForge, ID: account}, nil
	default:
		return Publisher{}, fmt.Errorf("%w: kind %q", ErrPublisher, kind)
	}
}

func forgeAccount(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: forge account is empty", ErrPublisher)
	}
	if !strings.Contains(s, "://") && strings.Count(s, "/") == 1 {
		host, user, _ := strings.Cut(s, "/")
		host = strings.ToLower(host)
		user = strings.TrimSpace(user)
		if host == "" || user == "" || strings.Contains(user, "/") {
			return "", fmt.Errorf("%w: forge account must be host/user", ErrPublisher)
		}
		return host + "/" + user, nil
	}
	repo, ok := repourl.Parse(s)
	if !ok || repo.User == "" {
		return "", fmt.Errorf("%w: forge account must be host/user", ErrPublisher)
	}
	return strings.ToLower(repo.Host) + "/" + repo.User, nil
}

// Blacklisted reports whether this publisher is banned.
func (s *Store) Blacklisted(ctx context.Context, pub Publisher) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist WHERE kind = ? AND id = ?`, pub.Kind, pub.ID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HashReason returns the operator text stored for a banned file hash.
func (s *Store) HashReason(ctx context.Context, hash string) (string, bool, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT reason FROM banned_hashes WHERE hash = ?`, hash).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return reason, true, nil
}

// RecordBan stores the publisher, file hashes, and the app ids removed with this reason.
func (s *Store) RecordBan(ctx context.Context, pub Publisher, reason string, hashes, appIDs []string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrReason
	}
	pub, err := Normalize(pub.Kind, pub.ID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO blacklist (kind, id, reason, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, id) DO UPDATE SET reason = excluded.reason
	`, pub.Kind, pub.ID, reason, now); err != nil {
		return err
	}
	for _, hash := range hashes {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if hash == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO banned_hashes (hash, reason) VALUES (?, ?)
			ON CONFLICT (hash) DO UPDATE SET reason = excluded.reason
		`, hash, reason); err != nil {
			return err
		}
	}
	for _, appID := range appIDs {
		appID = strings.TrimSpace(appID)
		if appID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ban_notices (app_id, reason) VALUES (?, ?)
			ON CONFLICT (app_id) DO UPDATE SET reason = excluded.reason
		`, appID, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Notices returns the operator text for each app id that has one.
func (s *Store) Notices(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_id, reason FROM ban_notices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var appID, reason string
		if err := rows.Scan(&appID, &reason); err != nil {
			return nil, err
		}
		out[appID] = reason
	}
	return out, rows.Err()
}

// PutSuggestion stores a suggestion. A blacklisted forge account or certificate is not stored.
// stored is false when the row was skipped. id is stable for the same input either way.
func (s *Store) PutSuggestion(ctx context.Context, repository, releaseSource, certificateHash string) (id string, stored bool, err error) {
	repository, err = normalizeURL(repository)
	if err != nil {
		return "", false, err
	}
	releaseSource, err = normalizeURL(releaseSource)
	if err != nil {
		return "", false, err
	}
	certificateHash = strings.TrimSpace(certificateHash)
	if repository == "" && releaseSource == "" {
		return "", false, ErrSuggest
	}
	var certID string
	if certificateHash != "" {
		pub, err := Normalize(KindCert, certificateHash)
		if err != nil {
			return "", false, err
		}
		certID = pub.ID
	}
	sum := sha256.Sum256([]byte(repository + "\n" + releaseSource + "\n" + certID))
	id = hex.EncodeToString(sum[:])

	if repository != "" {
		if account, err := forgeAccount(repository); err == nil {
			banned, err := s.Blacklisted(ctx, Publisher{Kind: KindForge, ID: account})
			if err != nil {
				return "", false, err
			}
			if banned {
				return id, false, nil
			}
		}
	}
	if certID != "" {
		banned, err := s.Blacklisted(ctx, Publisher{Kind: KindCert, ID: certID})
		if err != nil {
			return "", false, err
		}
		if banned {
			return id, false, nil
		}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO suggestions (id, repository, release_source, certificate_hash, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING
	`, id, repository, releaseSource, certID, time.Now().UTC().Unix())
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Suggestions lists the queue.
func (s *Store) Suggestions(ctx context.Context) ([]Suggestion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repository, release_source, certificate_hash
		FROM suggestions ORDER BY created_at, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var row Suggestion
		if err := rows.Scan(&row.ID, &row.Repository, &row.ReleaseSource, &row.CertificateHash); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if out == nil {
		out = []Suggestion{}
	}
	return out, rows.Err()
}

// Entry is one publisher on the blacklist.
type Entry struct {
	Kind      string
	ID        string
	Reason    string
	CreatedAt int64
}

// Publishers lists the blacklist, oldest first.
func (s *Store) Publishers(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, id, reason, created_at FROM blacklist
		ORDER BY created_at, kind, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var row Entry
		if err := rows.Scan(&row.Kind, &row.ID, &row.Reason, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if out == nil {
		out = []Entry{}
	}
	return out, rows.Err()
}

// RemovePublisher drops one publisher from the blacklist.
// Listings already deleted stay deleted. A missing row is success.
func (s *Store) RemovePublisher(ctx context.Context, kind, id string) error {
	pub, err := Normalize(kind, id)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM blacklist WHERE kind = ? AND id = ?`, pub.Kind, pub.ID)
	return err
}

// Hash is one banned file.
type Hash struct {
	Hash   string
	Reason string
}

// Hashes lists banned file hashes.
func (s *Store) Hashes(ctx context.Context) ([]Hash, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash, reason FROM banned_hashes ORDER BY hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hash
	for rows.Next() {
		var row Hash
		if err := rows.Scan(&row.Hash, &row.Reason); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if out == nil {
		out = []Hash{}
	}
	return out, rows.Err()
}

// RemoveHash drops one banned file hash. A missing hash is success.
func (s *Store) RemoveHash(ctx context.Context, hash string) error {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" {
		return errors.New("hash is empty")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM banned_hashes WHERE hash = ?`, hash)
	return err
}

// RemoveSuggestion deletes one queue row. A missing id is success.
func (s *Store) RemoveSuggestion(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("suggestion id is empty")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM suggestions WHERE id = ?`, id)
	return err
}

// Verdict is the admission decision for one event.
// Reason is set only when the event names a banned file hash. That rejects
// the event and does not blacklist the key that signed it.
type Verdict struct {
	Reject bool
	Reason string
}

// EventVerdict checks a listing event against the blacklist and banned hashes.
// A known file hash rejects the event. It does not blacklist the signer.
// Certificate tags are not banned from the event.
func (s *Store) EventVerdict(ctx context.Context, event *nostr.Event) (Verdict, error) {
	var v Verdict
	if event == nil {
		return v, nil
	}
	if ok, err := s.blacklistedID(ctx, KindNostr, event.PubKey); err != nil {
		return v, err
	} else if ok {
		v.Reject = true
		return v, nil
	}
	if repo, found := events.Find(event.Tags, "repository"); found {
		if account, err := forgeAccount(repo); err == nil {
			banned, err := s.Blacklisted(ctx, Publisher{Kind: KindForge, ID: account})
			if err != nil {
				return v, err
			}
			if banned {
				v.Reject = true
				return v, nil
			}
		}
	}
	for _, cert := range events.FindAll(event.Tags, "apk_certificate_hash") {
		pub, err := Normalize(KindCert, cert)
		if err != nil {
			continue
		}
		banned, err := s.Blacklisted(ctx, pub)
		if err != nil {
			return v, err
		}
		if banned {
			v.Reject = true
			return v, nil
		}
	}
	hash, found := events.Find(event.Tags, "x")
	if !found || event.Kind != events.KindAsset {
		return v, nil
	}
	reason, banned, err := s.HashReason(ctx, hash)
	if err != nil || !banned {
		return v, err
	}
	v.Reject = true
	v.Reason = reason
	return v, nil
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: invalid url", ErrSuggest)
	}
	return u.String(), nil
}

// Listing is one stored app, release, or asset event.
type Listing struct {
	ID     string
	Pubkey string
	Kind   int
	Tags   nostr.Tags
}

// Affected returns the event ids, file hashes, and app ids a ban of pub removes.
func Affected(pub Publisher, rows []Listing) (eventIDs, hashes, appIDs []string) {
	pub, err := Normalize(pub.Kind, pub.ID)
	if err != nil {
		return nil, nil, nil
	}
	want := map[string]struct{}{}
	switch pub.Kind {
	case KindNostr:
		for _, row := range rows {
			if row.Pubkey == pub.ID {
				want[row.ID] = struct{}{}
			}
		}
	case KindForge:
		for _, row := range rows {
			if row.Kind != events.KindApp {
				continue
			}
			repo, ok := events.Find(row.Tags, "repository")
			if !ok {
				continue
			}
			account, err := forgeAccount(repo)
			if err != nil || account != pub.ID {
				continue
			}
			appID, ok := events.Find(row.Tags, "d")
			if !ok {
				continue
			}
			markApp(want, rows, row.Pubkey, appID)
		}
	case KindCert:
		for _, row := range rows {
			if row.Kind != events.KindAsset {
				continue
			}
			for _, cert := range events.FindAll(row.Tags, "apk_certificate_hash") {
				norm, err := Normalize(KindCert, cert)
				if err != nil || norm.ID != pub.ID {
					continue
				}
				appID, ok := events.Find(row.Tags, "i")
				if !ok {
					continue
				}
				markApp(want, rows, row.Pubkey, appID)
			}
		}
	}
	seenHash := map[string]struct{}{}
	seenApp := map[string]struct{}{}
	for _, row := range rows {
		if _, ok := want[row.ID]; !ok {
			continue
		}
		eventIDs = append(eventIDs, row.ID)
		switch row.Kind {
		case events.KindApp:
			noteApp(seenApp, &appIDs, row.Tags, "d")
		case events.KindRelease:
			noteApp(seenApp, &appIDs, row.Tags, "i")
		case events.KindAsset:
			if hash, ok := events.Find(row.Tags, "x"); ok {
				hash = strings.ToLower(hash)
				if _, done := seenHash[hash]; !done {
					seenHash[hash] = struct{}{}
					hashes = append(hashes, hash)
				}
			}
			noteApp(seenApp, &appIDs, row.Tags, "i")
		}
	}
	return eventIDs, hashes, appIDs
}

func noteApp(seen map[string]struct{}, appIDs *[]string, tags nostr.Tags, key string) {
	appID, ok := events.Find(tags, key)
	if !ok {
		return
	}
	if _, done := seen[appID]; done {
		return
	}
	seen[appID] = struct{}{}
	*appIDs = append(*appIDs, appID)
}

func markApp(want map[string]struct{}, rows []Listing, pubkey, appID string) {
	for _, row := range rows {
		if row.Pubkey != pubkey {
			continue
		}
		switch row.Kind {
		case events.KindApp:
			if id, ok := events.Find(row.Tags, "d"); ok && id == appID {
				want[row.ID] = struct{}{}
			}
		case events.KindRelease, events.KindAsset:
			if id, ok := events.Find(row.Tags, "i"); ok && id == appID {
				want[row.ID] = struct{}{}
			}
		}
	}
}

func (s *Store) blacklistedID(ctx context.Context, kind, id string) (bool, error) {
	pub, err := Normalize(kind, id)
	if err != nil {
		return false, nil
	}
	return s.Blacklisted(ctx, pub)
}
