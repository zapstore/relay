// The store package is responsible for storing blobs metadata in sqlite.
// The actual blob data is stored somewhere else (e.g. Bunny CDN).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/dbcheck"
)

//go:embed schema.sql
var schema string

var (
	ErrBlobNotFound = errors.New("blob not found")
)

// State is where a blob is in the buffer zone.
type State string

const (
	// Pending is on local disk, waiting for its listing. It is not served.
	Pending State = "pending"
	// Moving is promoted and served from local disk until it is on Bunny.
	Moving State = "moving"
	// Served is on Bunny when Bunny is set, else on local disk.
	Served State = "served"
)

type T struct {
	DB *sql.DB
}

// BlobMeta holds metadata about a blob stored in the database.
type BlobMeta struct {
	Hash       blossom.Hash
	Type       string // MIME type
	Size       int64
	CreatedAt  time.Time
	AuthPubkey string // hex pubkey that authenticated the upload, empty if unknown
	State      State
}

// New creates a new store with the given path.
func New(path string) (*T, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to sqlite3 at %s: %w", path, err)
	}
	if err := dbcheck.RequireColumn(db, "blobs", "state"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("failed to apply base schema: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		return nil, fmt.Errorf("failed to set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return nil, fmt.Errorf("failed to activate foreign keys: %w", err)
	}
	if _, err = db.Exec("PRAGMA optimize=0x10002;"); err != nil {
		return nil, fmt.Errorf("failed to PRAGMA optimize: %w", err)
	}
	return &T{DB: db}, nil
}

func (s *T) Close() error {
	return s.DB.Close()
}

// Save saves the metadata of a blob to the database.
// Returns true if the blob was inserted, false if it already existed.
// If CreatedAt is zero, it defaults to the current UTC time. An empty State is Served.
func (s *T) Save(ctx context.Context, b BlobMeta) (inserted bool, err error) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	if b.State == "" {
		b.State = Served
	}

	var authPubkey *string
	if b.AuthPubkey != "" {
		authPubkey = &b.AuthPubkey
	}

	query := `INSERT OR IGNORE INTO blobs (hash, type, size, created_at, auth_pubkey, state) VALUES (?, ?, ?, ?, ?, ?)`
	res, err := s.DB.ExecContext(ctx, query, b.Hash, b.Type, b.Size, b.CreatedAt.Unix(), authPubkey, b.State)
	if err != nil {
		return false, fmt.Errorf("failed to save blob metadata: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return n > 0, nil
}

// Query retrieves the metadata of a blob from the database.
func (s *T) Query(ctx context.Context, hash blossom.Hash) (BlobMeta, error) {
	query := `SELECT hash, type, size, created_at, auth_pubkey, state FROM blobs WHERE hash = ?`
	meta, err := scanMeta(s.DB.QueryRowContext(ctx, query, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return BlobMeta{}, ErrBlobNotFound
	}
	if err != nil {
		return BlobMeta{}, fmt.Errorf("failed to get blob metadata: %w", err)
	}
	return meta, nil
}

// InState returns the blobs in state. A non-zero before keeps only blobs
// created before it.
func (s *T) InState(ctx context.Context, state State, before time.Time) ([]BlobMeta, error) {
	query := `SELECT hash, type, size, created_at, auth_pubkey, state FROM blobs WHERE state = ?`
	args := []any{state}
	if !before.IsZero() {
		query += ` AND created_at < ?`
		args = append(args, before.Unix())
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s blobs: %w", state, err)
	}
	defer rows.Close()
	var out []BlobMeta
	for rows.Next() {
		meta, err := scanMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan blob metadata: %w", err)
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

// SetState moves a blob from one state to another. It reports false when the
// blob is missing or not in from.
func (s *T) SetState(ctx context.Context, hash blossom.Hash, from, to State) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE blobs SET state = ? WHERE hash = ? AND state = ?`, to, hash, from)
	if err != nil {
		return false, fmt.Errorf("failed to set blob state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return n > 0, nil
}

// Renew restarts the buffer zone clock of a pending blob.
func (s *T) Renew(ctx context.Context, hash blossom.Hash, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blobs SET created_at = ? WHERE hash = ? AND state = ?`, at.Unix(), hash, Pending)
	if err != nil {
		return fmt.Errorf("failed to renew blob: %w", err)
	}
	return nil
}

// Delete removes a blob's metadata record from the database.
func (s *T) Delete(ctx context.Context, hash blossom.Hash) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, hash)
	if err != nil {
		return fmt.Errorf("failed to delete blob: %w", err)
	}
	return nil
}

// Has checks whether a blob with the given hash exists in the database.
func (s *T) Has(ctx context.Context, hash blossom.Hash) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`
	var exists bool
	err := s.DB.QueryRowContext(ctx, query, hash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if blob exists: %w", err)
	}
	return exists, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMeta(row scanner) (BlobMeta, error) {
	var hash string
	var meta BlobMeta
	var createdAt int64
	var authPubkey sql.NullString
	if err := row.Scan(&hash, &meta.Type, &meta.Size, &createdAt, &authPubkey, &meta.State); err != nil {
		return BlobMeta{}, err
	}
	parsed, err := blossom.ParseHash(hash)
	if err != nil {
		return BlobMeta{}, err
	}
	meta.Hash = parsed
	meta.CreatedAt = time.Unix(createdAt, 0).UTC()
	if authPubkey.Valid {
		meta.AuthPubkey = authPubkey.String
	}
	return meta, nil
}
