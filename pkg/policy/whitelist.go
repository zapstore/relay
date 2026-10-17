package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr/nip19"
)

// AllowedKey is one Nostr pubkey on the whitelist.
type AllowedKey struct {
	ID        string
	CreatedAt int64
}

// nostrKey accepts a 64-character hex pubkey or an npub.
func nostrKey(id string) (string, error) {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "npub1") {
		prefix, v, err := nip19.Decode(id)
		if err != nil || prefix != "npub" {
			return "", fmt.Errorf("%w: invalid npub", ErrPublisher)
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%w: invalid npub", ErrPublisher)
		}
		id = s
	}
	pub, err := Normalize(KindNostr, id)
	if err != nil {
		return "", err
	}
	return pub.ID, nil
}

// Allow adds a Nostr pubkey to the whitelist. A second add keeps the first time.
// Only a Nostr key is accepted. A blacklist row still rejects the key.
func (s *Store) Allow(ctx context.Context, pubkey string) error {
	id, err := nostrKey(pubkey)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO whitelist (kind, id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (kind, id) DO NOTHING
	`, KindNostr, id, time.Now().UTC().Unix())
	return err
}

// Disallow removes a Nostr pubkey from the whitelist. A missing row is success.
func (s *Store) Disallow(ctx context.Context, pubkey string) error {
	id, err := nostrKey(pubkey)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM whitelist WHERE kind = ? AND id = ?`, KindNostr, id)
	return err
}

// Allowed reports whether pubkey is on the whitelist.
func (s *Store) Allowed(ctx context.Context, pubkey string) (bool, error) {
	id, err := nostrKey(pubkey)
	if err != nil {
		return false, err
	}
	var one int
	err = s.db.QueryRowContext(ctx, `
		SELECT 1 FROM whitelist WHERE kind = ? AND id = ? LIMIT 1
	`, KindNostr, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AllowedKeys lists whitelisted pubkeys, newest first.
func (s *Store) AllowedKeys(ctx context.Context) ([]AllowedKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at FROM whitelist WHERE kind = ?
		ORDER BY created_at DESC, id
	`, KindNostr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllowedKey
	for rows.Next() {
		var row AllowedKey
		if err := rows.Scan(&row.ID, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if out == nil {
		out = []AllowedKey{}
	}
	return out, rows.Err()
}
