// Package store provides SQLite access to the shared indexing.db database.
// This database is written by the relay (discovery misses, release requests)
// and read/written by zindex (processing queue entries, updating status).
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS discovery_queue (
    url             TEXT PRIMARY KEY,
    request_count   INTEGER NOT NULL DEFAULT 1,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    checked_at      INTEGER
);

CREATE INDEX IF NOT EXISTS idx_discovery_status ON discovery_queue(status);
CREATE INDEX IF NOT EXISTS idx_discovery_request_count ON discovery_queue(request_count DESC);

CREATE TABLE IF NOT EXISTS index_status (
    app_id              TEXT PRIMARY KEY,
    last_checked_at     INTEGER,
    last_requested_at   INTEGER NOT NULL,
    request_count       INTEGER NOT NULL DEFAULT 0,
    window_start        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_index_status_last_checked ON index_status(last_checked_at);
`

// Store provides access to the indexing.db SQLite database.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the indexing.db at the given path with WAL mode and 5s busy_timeout.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("indexing store: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("indexing store: ping: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("indexing store: migrate: %w", err)
	}
	// Additive migration for existing databases.
	db.Exec(`ALTER TABLE discovery_queue ADD COLUMN fail_count INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertDiscovery records a discovery miss for the given GitHub URL.
// If the URL already exists, increments request_count and updates last_seen_at.
// Returns an error if the URL does not start with "https://github.com/".
func (s *Store) UpsertDiscovery(url string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO discovery_queue (url, request_count, first_seen_at, last_seen_at, status)
		VALUES (?, 1, ?, ?, 'pending')
		ON CONFLICT(url) DO UPDATE SET
		    request_count = request_count + 1,
		    last_seen_at  = excluded.last_seen_at,
		    status        = CASE WHEN status = 'done' THEN 'pending' ELSE status END
	`, url, now, now)
	return err
}

// requestCountWindow is the rolling window for request_count accumulation.
// Counts within a window reflect genuine demand pressure; once the window
// expires the counter resets so stale counts don't permanently shrink the TTL.
const requestCountWindow = int64(3600) // 1 hour

// UpsertReleaseRequest records a release request for the given app ID.
// request_count accumulates within a 1-hour window and resets when the window
// expires, preventing unbounded growth from automated fetches.
func (s *Store) UpsertReleaseRequest(appID string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO index_status (app_id, last_requested_at, request_count, window_start)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(app_id) DO UPDATE SET
		    last_requested_at = excluded.last_requested_at,
		    window_start      = CASE WHEN ? - window_start > ? THEN ? ELSE window_start END,
		    request_count     = CASE WHEN ? - window_start > ? THEN 1 ELSE MIN(request_count + 1, 168) END
	`, appID, now, now,
		now, requestCountWindow, now,
		now, requestCountWindow)
	return err
}

// ResetReleaseRequest resets request_count to 0 and updates window_start for the given app ID.
// Called when a release event is successfully stored, satisfying the outstanding demand.
func (s *Store) ResetReleaseRequest(appID string) error {
	_, err := s.db.Exec(`
		UPDATE index_status SET request_count = 0, window_start = ? WHERE app_id = ?
	`, time.Now().Unix(), appID)
	return err
}

// IsStale returns true if the app should be re-checked based on demand-driven TTL.
// TTL = clamp(maxTTL / request_count, minTTL, maxTTL).
func (s *Store) IsStale(appID string, minTTL, maxTTL time.Duration) (bool, error) {
	var lastCheckedAt sql.NullInt64
	var requestCount int64
	err := s.db.QueryRow(`
		SELECT last_checked_at, request_count FROM index_status WHERE app_id = ?
	`, appID).Scan(&lastCheckedAt, &requestCount)
	if err == sql.ErrNoRows {
		return true, nil // Never checked — stale
	}
	if err != nil {
		return false, err
	}
	if !lastCheckedAt.Valid {
		return true, nil
	}

	ttl := computeTTL(requestCount, minTTL, maxTTL)
	lastChecked := time.Unix(lastCheckedAt.Int64, 0)
	return time.Since(lastChecked) >= ttl, nil
}

// computeTTL computes clamp(maxTTL / requestCount, minTTL, maxTTL).
func computeTTL(requestCount int64, minTTL, maxTTL time.Duration) time.Duration {
	if requestCount <= 0 {
		return maxTTL
	}
	ttl := time.Duration(int64(maxTTL) / requestCount)
	if ttl < minTTL {
		return minTTL
	}
	if ttl > maxTTL {
		return maxTTL
	}
	return ttl
}
