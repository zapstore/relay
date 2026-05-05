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
)

//go:embed schema.sql
var schema string

var (
	ErrBlobNotFound = errors.New("blob not found")
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
	AuthPubkey string     // hex pubkey that authenticated the upload, empty if unknown
	ClaimedAt  *time.Time // when first confirmed referenced by a live event, nil = unclaimed (GC candidate)
}

// New creates a new store with the given path.
func New(path string) (*T, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to sqlite3 at %s: %w", path, err)
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
// If CreatedAt is zero, it defaults to the current UTC time.
func (s *T) Save(ctx context.Context, b BlobMeta) (inserted bool, err error) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}

	query := `INSERT OR IGNORE INTO blobs (hash, type, size, created_at) VALUES (?, ?, ?, ?)`
	res, err := s.DB.ExecContext(ctx, query, b.Hash, b.Type, b.Size, b.CreatedAt.Unix())
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
	var mime string
	var size int64
	var createdAt int64
	var authPubkey sql.NullString
	var claimedAt sql.NullInt64

	query := `SELECT type, size, created_at, auth_pubkey, claimed_at FROM blobs WHERE hash = ?`
	err := s.DB.QueryRowContext(ctx, query, hash).Scan(&mime, &size, &createdAt, &authPubkey, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobMeta{}, ErrBlobNotFound
	}
	if err != nil {
		return BlobMeta{}, fmt.Errorf("failed to get blob metadata: %w", err)
	}

	meta := BlobMeta{
		Hash:      hash,
		Type:      mime,
		Size:      size,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
	}
	if authPubkey.Valid {
		meta.AuthPubkey = authPubkey.String
	}
	if claimedAt.Valid {
		t := time.Unix(claimedAt.Int64, 0).UTC()
		meta.ClaimedAt = &t
	}
	return meta, nil
}

// Claim marks a blob as referenced by a live event, setting claimed_at to now if not already set.
// It is a no-op if the blob does not exist or is already claimed.
func (s *T) Claim(ctx context.Context, hash blossom.Hash) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE blobs SET claimed_at = ? WHERE hash = ? AND claimed_at IS NULL`,
		time.Now().UTC().Unix(), hash,
	)
	if err != nil {
		return fmt.Errorf("failed to claim blob: %w", err)
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
