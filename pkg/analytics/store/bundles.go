package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UnknownEpoch is stored when a bundle request never established that epoch.
const UnknownEpoch int64 = -1

// BundleRequest is one aggregated catalog bundle response.
type BundleRequest struct {
	Day           string // YYYY-MM-DD
	FromEpoch     int64
	ToEpoch       int64
	Status        int
	Protocol      string
	ClientVersion string
}

// BundleCount is a BundleRequest paired with its occurrence count.
type BundleCount struct {
	BundleRequest
	Count int
}

// SaveBundles writes the given batch of counted bundle requests.
// On conflict it increments the existing count. An empty batch is a no-op.
func (s *T) SaveBundles(ctx context.Context, batch []BundleCount) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO bundle_requests (day, from_epoch, to_epoch, status, protocol, client_version, count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, from_epoch, to_epoch, status, protocol, client_version)
		DO UPDATE SET count = bundle_requests.count + excluded.count
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, row := range batch {
		if _, err := stmt.ExecContext(
			ctx,
			row.Day,
			row.FromEpoch,
			row.ToEpoch,
			row.Status,
			row.Protocol,
			row.ClientVersion,
			row.Count,
		); err != nil {
			return fmt.Errorf("failed to execute statement: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// BundleFilter defines query parameters for QueryBundles.
// Epoch and status filters are decimal strings so 0 and -1 stay distinct from "unset".
type BundleFilter struct {
	From          string // YYYY-MM-DD, inclusive
	To            string // YYYY-MM-DD, inclusive
	FromEpoch     string
	ToEpoch       string
	Status        string
	Protocol      string
	ClientVersion string
	GroupBy       []string // subset of: day, from_epoch, to_epoch, status, protocol, client_version
}

var bundleGroupBy = []string{"day", "from_epoch", "to_epoch", "status", "protocol", "client_version"}

func (f BundleFilter) Validate() error {
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
	if f.FromEpoch != "" {
		if _, err := strconv.ParseInt(f.FromEpoch, 10, 64); err != nil {
			return fmt.Errorf("invalid from_epoch: %w", err)
		}
	}
	if f.ToEpoch != "" {
		if _, err := strconv.ParseInt(f.ToEpoch, 10, 64); err != nil {
			return fmt.Errorf("invalid to_epoch: %w", err)
		}
	}
	if f.Status != "" {
		n, err := strconv.Atoi(f.Status)
		if err != nil || n < 100 || n > 599 {
			return fmt.Errorf("invalid status: %s", f.Status)
		}
	}
	for _, g := range f.GroupBy {
		if !slices.Contains(bundleGroupBy, g) {
			return fmt.Errorf("invalid group_by: %s", g)
		}
	}
	return nil
}

// QueryBundles returns aggregated bundle request counts matching the filter.
// If GroupBy is empty, a single total-count row is returned.
func (s *T) QueryBundles(ctx context.Context, f BundleFilter) ([]BundleCount, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	query, args := queryBundlesSQL(f)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query bundle requests: %w", err)
	}
	defer rows.Close()

	var out []BundleCount
	for rows.Next() {
		var row BundleCount
		targets := bundleScanTargets(&row, f.GroupBy)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("failed to scan bundle request row: %w", err)
		}
		row.Day = normalizeDay(row.Day)
		out = append(out, row)
	}
	return out, rows.Err()
}

func queryBundlesSQL(f BundleFilter) (string, []any) {
	columns := make([]string, 0, len(f.GroupBy)+1)
	columns = append(columns, f.GroupBy...)
	columns = append(columns, "COALESCE(SUM(count), 0) AS count")
	query := "SELECT " + strings.Join(columns, ", ") + " FROM bundle_requests"

	var conds []string
	var args []any
	if f.From != "" {
		conds = append(conds, "day >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "day <= ?")
		args = append(args, f.To)
	}
	if f.FromEpoch != "" {
		conds = append(conds, "from_epoch = ?")
		args = append(args, f.FromEpoch)
	}
	if f.ToEpoch != "" {
		conds = append(conds, "to_epoch = ?")
		args = append(args, f.ToEpoch)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Protocol != "" {
		conds = append(conds, "protocol = ?")
		args = append(args, f.Protocol)
	}
	if f.ClientVersion != "" {
		conds = append(conds, "client_version = ?")
		args = append(args, f.ClientVersion)
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

func bundleScanTargets(row *BundleCount, cols []string) []any {
	targets := make([]any, 0, len(cols)+1)
	for _, col := range cols {
		switch col {
		case "day":
			targets = append(targets, &row.Day)
		case "from_epoch":
			targets = append(targets, &row.FromEpoch)
		case "to_epoch":
			targets = append(targets, &row.ToEpoch)
		case "status":
			targets = append(targets, &row.Status)
		case "protocol":
			targets = append(targets, &row.Protocol)
		case "client_version":
			targets = append(targets, &row.ClientVersion)
		}
	}
	return append(targets, &row.Count)
}
