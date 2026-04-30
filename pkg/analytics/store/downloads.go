package store

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
)

type DownloadType string

const (
	Install DownloadType = "install"
	Update  DownloadType = "update"
	Unknown DownloadType = "unknown"
)

func (d DownloadType) IsValid() bool {
	return d == Install || d == Update || d == Unknown
}

// Download of a blossom blob.
type Download struct {
	Hash        blossom.Hash
	AppID       string
	AppVersion  string
	AppPubkey   string
	Day         string // formatted as "YYYY-MM-DD"
	Source      Source
	Type        DownloadType
	CountryCode string // ISO 2 letter code
}

// DownloadCount is a Download paired with its occurrence count.
type DownloadCount struct {
	Download
	Count int
}

// ParseDownloadSource returns the Source derived from the request headers.
func ParseDownloadSource(h http.Header) Source {
	switch h.Get("X-Zapstore-Client") {
	case "app":
		return SourceApp
	case "web":
		return SourceWeb
	default:
		return SourceUnknown
	}
}

// ParseDownloadType returns the DownloadType derived from the request headers.
func ParseDownloadType(h http.Header) DownloadType {
	switch h.Get("X-Zapstore-Download-Type") {
	case "update":
		return Update
	case "install":
		return Install
	default:
		return Unknown
	}
}

// SaveDownloads writes the given batch of counted downloads to the database.
// On conflict it increments the existing count. An empty batch is a no-op.
func (s *T) SaveDownloads(ctx context.Context, batch []DownloadCount) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO downloads (hash, app_id, app_version, app_pubkey, day, source, type, country_code, count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash, day, source, type, country_code)
		DO UPDATE SET count = downloads.count + excluded.count
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, download := range batch {
		if _, err := stmt.ExecContext(
			ctx,
			download.Hash,
			download.AppID,
			download.AppVersion,
			download.AppPubkey,
			download.Day,
			string(download.Source),
			string(download.Type),
			download.CountryCode,
			download.Count,
		); err != nil {
			return fmt.Errorf("failed to execute statement: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// DownloadFilter defines query parameters for QueryDownloads.
type DownloadFilter struct {
	Hash      string       // restricts to a specific blob hash
	AppID     string       // restricts to a specific app
	AppPubkey string       // restricts to a specific publisher
	Type      DownloadType // restricts to a specific download type
	From      string       // YYYY-MM-DD, inclusive
	To        string       // YYYY-MM-DD, inclusive
	Source    Source       // restricts to a specific source
	GroupBy   []string     // subset of: hash, app_id, app_version, app_pubkey, day, source, type, country_code
}

var downloadGroupBy = []string{"hash", "app_id", "app_version", "app_pubkey", "day", "source", "type", "country_code"}

func (f DownloadFilter) Validate() error {
	if f.Hash != "" {
		if err := blossom.ValidateHash(f.Hash); err != nil {
			return fmt.Errorf("invalid hash: %w", err)
		}
	}
	if f.AppPubkey != "" {
		if !nostr.IsValidPublicKey(f.AppPubkey) {
			return fmt.Errorf("invalid app_pubkey: %s", f.AppPubkey)
		}
	}
	if f.Type != "" {
		if !f.Type.IsValid() {
			return fmt.Errorf("invalid type: %s", f.Type)
		}
	}
	if f.From != "" {
		if _, err := time.Parse("2006-01-02", f.From); err != nil {
			return fmt.Errorf("invalid from: %w", err)
		}
	}
	if f.To != "" {
		if _, err := time.Parse("2006-01-02", f.To); err != nil {
			return fmt.Errorf("invalid to: %w", err)
		}
	}
	if f.Source != "" {
		if !f.Source.IsValid() {
			return fmt.Errorf("invalid source: %s", f.Source)
		}
	}
	for _, g := range f.GroupBy {
		if !slices.Contains(downloadGroupBy, g) {
			return fmt.Errorf("invalid group_by: %s", g)
		}
	}
	return nil
}

// QueryDownloads returns aggregated download counts matching the given filter.
// If GroupBy is empty, a single total-count row is returned.
func (s *T) QueryDownloads(ctx context.Context, f DownloadFilter) ([]DownloadCount, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	query, args := queryDownloadsSQL(f)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query downloads: %w", err)
	}
	defer rows.Close()

	var downloads []DownloadCount
	for rows.Next() {
		var d DownloadCount
		targets := downloadScanTargets(&d, f.GroupBy)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("failed to scan download row: %w", err)
		}
		d.Day = normalizeDay(d.Day)
		downloads = append(downloads, d)
	}
	return downloads, rows.Err()
}

func queryDownloadsSQL(f DownloadFilter) (string, []any) {
	columns := make([]string, 0, len(f.GroupBy)+1)
	columns = append(columns, f.GroupBy...)
	columns = append(columns, "SUM(count) AS count")
	query := "SELECT " + strings.Join(columns, ", ") + " FROM downloads"

	var conds []string
	var args []any
	if f.Hash != "" {
		conds = append(conds, "hash = ?")
		args = append(args, f.Hash)
	}
	if f.AppID != "" {
		conds = append(conds, "app_id = ?")
		args = append(args, f.AppID)
	}
	if f.AppPubkey != "" {
		conds = append(conds, "app_pubkey = ?")
		args = append(args, f.AppPubkey)
	}
	if f.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, f.Type)
	}
	if f.From != "" {
		conds = append(conds, "day >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "day <= ?")
		args = append(args, f.To)
	}
	if f.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	if len(f.GroupBy) > 0 {
		query += " GROUP BY " + strings.Join(f.GroupBy, ", ")
	}
	for _, c := range f.GroupBy {
		if c == "day" {
			query += " ORDER BY day DESC"
		}
	}
	return query, args
}

// downloadScanTargets returns scan destinations matching the SELECT column order.
func downloadScanTargets(row *DownloadCount, dbCols []string) []any {
	targets := make([]any, 0, len(dbCols)+1)
	for _, col := range dbCols {
		switch col {
		case "hash":
			targets = append(targets, &row.Hash)
		case "day":
			targets = append(targets, &row.Day)
		case "app_id":
			targets = append(targets, &row.AppID)
		case "app_version":
			targets = append(targets, &row.AppVersion)
		case "app_pubkey":
			targets = append(targets, &row.AppPubkey)
		case "source":
			targets = append(targets, &row.Source)
		case "type":
			targets = append(targets, &row.Type)
		case "country_code":
			targets = append(targets, &row.CountryCode)
		}
	}
	return append(targets, &row.Count)
}
