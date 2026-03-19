package store

import (
	"context"
	"fmt"
	"strings"
)

// impressionGroupBy maps user-facing group_by names to DB column names for the impressions table.
var impressionGroupBy = map[string]string{
	"app_id":  "app_id",
	"pubkey":  "app_pubkey",
	"day":     "day",
	"source":  "source",
	"type":    "type",
	"country": "country_code",
}

// downloadGroupBy maps user-facing group_by names to DB column names for the downloads table.
var downloadGroupBy = map[string]string{
	"hash":    "hash",
	"day":     "day",
	"source":  "source",
	"country": "country_code",
}

// ImpressionFilter defines query parameters for QueryImpressions.
type ImpressionFilter struct {
	Pubkey  string   // optional; restricts to a specific app publisher
	From    string   // YYYY-MM-DD, inclusive
	To      string   // YYYY-MM-DD, inclusive
	GroupBy []string // subset of: app_id, pubkey, day, source, type, country
}

// ImpressionRow is a single result row from QueryImpressions.
// Fields not included in GroupBy are empty (aggregated away).
type ImpressionRow struct {
	AppID   string `json:"app_id,omitempty"`
	Pubkey  string `json:"pubkey,omitempty"`
	Day     string `json:"day,omitempty"`
	Source  string `json:"source,omitempty"`
	Type    string `json:"type,omitempty"`
	Country string `json:"country,omitempty"`
	Count   int64  `json:"count"`
}

// DownloadFilter defines query parameters for QueryDownloads.
type DownloadFilter struct {
	Hash    string   // optional; restricts to a specific blob hash
	From    string   // YYYY-MM-DD, inclusive
	To      string   // YYYY-MM-DD, inclusive
	GroupBy []string // subset of: hash, day, source, country
}

// DownloadRow is a single result row from QueryDownloads.
// Fields not included in GroupBy are empty (aggregated away).
type DownloadRow struct {
	Hash    string `json:"hash,omitempty"`
	Day     string `json:"day,omitempty"`
	Source  string `json:"source,omitempty"`
	Country string `json:"country,omitempty"`
	Count   int64  `json:"count"`
}

// QueryImpressions returns aggregated impression counts matching the given filter.
// If GroupBy is empty, a single total-count row is returned.
func (s *Store) QueryImpressions(ctx context.Context, f ImpressionFilter) ([]ImpressionRow, error) {
	dbCols, err := resolveGroupBy(f.GroupBy, impressionGroupBy)
	if err != nil {
		return nil, err
	}

	selectParts := append(dbCols, "SUM(count) AS count")
	q := "SELECT " + strings.Join(selectParts, ", ") + " FROM impressions"

	var args []any
	var conds []string
	if f.Pubkey != "" {
		conds = append(conds, "app_pubkey = ?")
		args = append(args, f.Pubkey)
	}
	if f.From != "" {
		conds = append(conds, "day >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "day <= ?")
		args = append(args, f.To)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if len(dbCols) > 0 {
		q += " GROUP BY " + strings.Join(dbCols, ", ")
	}
	q += orderBy(dbCols)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query impressions: %w", err)
	}
	defer rows.Close()

	var result []ImpressionRow
	for rows.Next() {
		var row ImpressionRow
		targets := impressionScanTargets(&row, dbCols)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("failed to scan impression row: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// QueryDownloads returns aggregated download counts matching the given filter.
// If GroupBy is empty, a single total-count row is returned.
func (s *Store) QueryDownloads(ctx context.Context, f DownloadFilter) ([]DownloadRow, error) {
	dbCols, err := resolveGroupBy(f.GroupBy, downloadGroupBy)
	if err != nil {
		return nil, err
	}

	selectParts := append(dbCols, "SUM(count) AS count")
	q := "SELECT " + strings.Join(selectParts, ", ") + " FROM downloads"

	var args []any
	var conds []string
	if f.Hash != "" {
		conds = append(conds, "hash = ?")
		args = append(args, f.Hash)
	}
	if f.From != "" {
		conds = append(conds, "day >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "day <= ?")
		args = append(args, f.To)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if len(dbCols) > 0 {
		q += " GROUP BY " + strings.Join(dbCols, ", ")
	}
	q += orderBy(dbCols)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query downloads: %w", err)
	}
	defer rows.Close()

	var result []DownloadRow
	for rows.Next() {
		var row DownloadRow
		targets := downloadScanTargets(&row, dbCols)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("failed to scan download row: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// QueryRelayMetrics returns daily relay metrics for the given date range.
func (s *Store) QueryRelayMetrics(ctx context.Context, from, to string) ([]RelayMetrics, error) {
	q, args := dateRangeQuery("SELECT day, reqs, filters, events FROM relay_metrics", from, to)
	q += " ORDER BY day DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query relay metrics: %w", err)
	}
	defer rows.Close()

	var result []RelayMetrics
	for rows.Next() {
		var m RelayMetrics
		if err := rows.Scan(&m.Day, &m.Reqs, &m.Filters, &m.Events); err != nil {
			return nil, fmt.Errorf("failed to scan relay metrics row: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// QueryBlossomMetrics returns daily blossom metrics for the given date range.
func (s *Store) QueryBlossomMetrics(ctx context.Context, from, to string) ([]BlossomMetrics, error) {
	q, args := dateRangeQuery("SELECT day, checks, downloads, uploads FROM blossom_metrics", from, to)
	q += " ORDER BY day DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query blossom metrics: %w", err)
	}
	defer rows.Close()

	var result []BlossomMetrics
	for rows.Next() {
		var m BlossomMetrics
		if err := rows.Scan(&m.Day, &m.Checks, &m.Downloads, &m.Uploads); err != nil {
			return nil, fmt.Errorf("failed to scan blossom metrics row: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// resolveGroupBy validates user-supplied group_by keys and returns the corresponding DB column names.
func resolveGroupBy(keys []string, allowed map[string]string) ([]string, error) {
	cols := make([]string, 0, len(keys))
	for _, k := range keys {
		col, ok := allowed[k]
		if !ok {
			return nil, fmt.Errorf("unknown group_by value %q", k)
		}
		cols = append(cols, col)
	}
	return cols, nil
}

// orderBy returns an ORDER BY clause that sorts by day DESC when day is present, else empty.
func orderBy(dbCols []string) string {
	for _, c := range dbCols {
		if c == "day" {
			return " ORDER BY day DESC"
		}
	}
	return ""
}

// dateRangeQuery appends optional WHERE day >= / day <= clauses to base.
func dateRangeQuery(base, from, to string) (string, []any) {
	var conds []string
	var args []any
	if from != "" {
		conds = append(conds, "day >= ?")
		args = append(args, from)
	}
	if to != "" {
		conds = append(conds, "day <= ?")
		args = append(args, to)
	}
	if len(conds) > 0 {
		base += " WHERE " + strings.Join(conds, " AND ")
	}
	return base, args
}

// impressionScanTargets returns scan destinations matching the SELECT column order.
func impressionScanTargets(row *ImpressionRow, dbCols []string) []any {
	targets := make([]any, 0, len(dbCols)+1)
	for _, col := range dbCols {
		switch col {
		case "app_id":
			targets = append(targets, &row.AppID)
		case "app_pubkey":
			targets = append(targets, &row.Pubkey)
		case "day":
			targets = append(targets, &row.Day)
		case "source":
			targets = append(targets, &row.Source)
		case "type":
			targets = append(targets, &row.Type)
		case "country_code":
			targets = append(targets, &row.Country)
		}
	}
	return append(targets, &row.Count)
}

// downloadScanTargets returns scan destinations matching the SELECT column order.
func downloadScanTargets(row *DownloadRow, dbCols []string) []any {
	targets := make([]any, 0, len(dbCols)+1)
	for _, col := range dbCols {
		switch col {
		case "hash":
			targets = append(targets, &row.Hash)
		case "day":
			targets = append(targets, &row.Day)
		case "source":
			targets = append(targets, &row.Source)
		case "country_code":
			targets = append(targets, &row.Country)
		}
	}
	return append(targets, &row.Count)
}
