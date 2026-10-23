package store

import (
	"context"
	"fmt"
	"strings"
)

// RelayMetrics holds aggregated relay counters for a single day.
type RelayMetrics struct {
	Day     string // formatted as "YYYY-MM-DD"
	Reqs    int64  // REQs fulfilled
	Filters int64  // filters fulfilled
	Events  int64  // events saved or replaced
}

// SaveRelayMetrics writes the given relay metrics to the database for the given day.
// On conflict it increments the existing counters.
func (s *T) SaveRelayMetrics(ctx context.Context, m RelayMetrics) error {
	if m.Reqs == 0 && m.Filters == 0 && m.Events == 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO relay_metrics (day, reqs, filters, events)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(day)
		DO UPDATE SET
			reqs    = relay_metrics.reqs    + excluded.reqs,
			filters = relay_metrics.filters + excluded.filters,
			events  = relay_metrics.events  + excluded.events
	`, m.Day, m.Reqs, m.Filters, m.Events)
	if err != nil {
		return fmt.Errorf("failed to save relay metrics: %w", err)
	}
	return nil
}

// QueryRelayMetrics returns daily relay metrics for the given date range.
func (s *T) QueryRelayMetrics(ctx context.Context, from, to string) ([]RelayMetrics, error) {
	query := "SELECT day, reqs, filters, events FROM relay_metrics"
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
		query += " WHERE " + strings.Join(conds, " AND ")
	}

	query += " ORDER BY day DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
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
		m.Day = normalizeDay(m.Day)
		result = append(result, m)
	}
	return result, rows.Err()
}
