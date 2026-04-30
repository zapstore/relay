package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	eventPkg "github.com/zapstore/relay/pkg/events"
)

// Source represents where the impression was made.
type Source string

const (
	SourceApp     Source = "app"
	SourceWeb     Source = "web"
	SourceUnknown Source = "unknown"
)

func (s Source) IsValid() bool {
	switch s {
	case SourceApp, SourceWeb, SourceUnknown:
		return true
	default:
		return false
	}
}

// ImpressionType represents the type of impression, which is determined by the REQ.
type ImpressionType string

const ImpressionDetail ImpressionType = "detail"

func (t ImpressionType) IsValid() bool {
	switch t {
	case ImpressionDetail:
		return true
	default:
		return false
	}
}

// Impression of an app.
type Impression struct {
	AppID       string
	AppPubkey   string
	Day         string // formatted as "YYYY-MM-DD"
	Source      Source
	Type        ImpressionType
	CountryCode string // ISO 2 letter code
}

// ImpressionCount is an Impression paired with its occurrence count.
type ImpressionCount struct {
	Impression
	Count int
}

// ImpressionSource returns the Source derived from the REQ subscription id.
func ImpressionSource(id string) Source {
	switch {
	case strings.HasPrefix(id, "app"):
		return SourceApp
	case strings.HasPrefix(id, "web-app"):
		return SourceWeb
	default:
		return SourceUnknown
	}
}

// IsDetailFilter reports whether the filter represents an app detail view
func IsDetailFilter(filter nostr.Filter) bool {
	for _, k := range filter.Kinds {
		if k == eventPkg.KindApp && len(filter.Tags["d"]) > 0 {
			return true
		}
	}
	return false
}

// NewImpressions creates impressions from an app detail REQ.
// Only known sources (app, web) and detail filters (kind 32267 + d tag) produce impressions.
func NewImpressions(country string, id string, filters nostr.Filters, events []nostr.Event) []Impression {
	source := ImpressionSource(id)
	day := Today()
	impressions := make([]Impression, 0, len(events))

	for _, f := range filters {
		if !IsDetailFilter(f) {
			continue
		}

		for _, event := range matchingEvents(f, events) {
			appID := event.Tags.GetD()
			if appID == "" {
				continue
			}
			impressions = append(impressions, Impression{
				AppID:       appID,
				AppPubkey:   event.PubKey,
				Day:         day,
				Source:      source,
				Type:        ImpressionDetail,
				CountryCode: country,
			})
		}
	}
	return impressions
}

// Today returns the current day formatted as "YYYY-MM-DD".
func Today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// matchingEvents returns the subset of events that match the given filter.
func matchingEvents(f nostr.Filter, events []nostr.Event) []nostr.Event {
	var matched []nostr.Event
	for _, e := range events {
		if f.Matches(&e) {
			matched = append(matched, e)
		}
	}
	return matched
}

// SaveImpressions writes the given batch of counted impressions to the database.
// On conflict it increments the existing count. An empty batch is a no-op.
func (s *Store) SaveImpressions(ctx context.Context, batch []ImpressionCount) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO impressions (app_id, app_pubkey, day, source, type, country_code, count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, app_pubkey, day, source, type, country_code)
		DO UPDATE SET count = impressions.count + excluded.count
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, impression := range batch {
		if _, err := stmt.ExecContext(
			ctx,
			impression.AppID,
			impression.AppPubkey,
			impression.Day,
			string(impression.Source),
			string(impression.Type),
			impression.CountryCode,
			impression.Count,
		); err != nil {
			return fmt.Errorf("failed to execute statement: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// ImpressionFilter defines query parameters for QueryImpressions.
type ImpressionFilter struct {
	AppID     string         // restricts to a specific app
	AppPubkey string         // restricts to a specific app publisher
	From      string         // YYYY-MM-DD, inclusive
	To        string         // YYYY-MM-DD, inclusive
	Source    Source         // restricts to a specific source
	Type      ImpressionType // restricts to a specific type
	GroupBy   []string       // subset of: app_id, app_pubkey, day, source, type, country_code
}

var impressionGroupBy = []string{"app_id", "app_pubkey", "day", "source", "type", "country_code"}

func (f ImpressionFilter) Validate() error {
	if f.AppPubkey != "" {
		if !nostr.IsValidPublicKey(f.AppPubkey) {
			return fmt.Errorf("invalid app pubkey: %s", f.AppPubkey)
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
	if f.Type != "" {
		if !f.Type.IsValid() {
			return fmt.Errorf("invalid type: %s", f.Type)
		}
	}
	for _, g := range f.GroupBy {
		if !slices.Contains(impressionGroupBy, g) {
			return fmt.Errorf("invalid group_by: %s", g)
		}
	}
	return nil
}

// QueryImpressions returns aggregated impression counts matching the given filter.
// If GroupBy is empty, a single total-count row is returned.
func (s *Store) QueryImpressions(ctx context.Context, f ImpressionFilter) ([]ImpressionCount, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	query, args := queryImpressionsSQL(f)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query impressions: %w", err)
	}
	defer rows.Close()

	var impressions []ImpressionCount
	for rows.Next() {
		var i ImpressionCount
		targets := impressionScan(&i, f.GroupBy)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("failed to scan impression row: %w", err)
		}
		i.Day = normalizeDay(i.Day)
		impressions = append(impressions, i)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to query impressions: %w", err)
	}
	return impressions, nil
}

// queryImpressionsSQL returns the SQL query and arguments for the given filter.
func queryImpressionsSQL(f ImpressionFilter) (string, []any) {
	columns := make([]string, 0, len(f.GroupBy)+1)
	columns = append(columns, f.GroupBy...)
	columns = append(columns, "SUM(count) AS count")
	query := "SELECT " + strings.Join(columns, ", ") + " FROM impressions"

	var args []any
	var conds []string
	if f.AppID != "" {
		conds = append(conds, "app_id = ?")
		args = append(args, f.AppID)
	}
	if f.AppPubkey != "" {
		conds = append(conds, "app_pubkey = ?")
		args = append(args, f.AppPubkey)
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
	if f.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, f.Type)
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

// impressionScan returns scan destinations matching the SELECT column order.
func impressionScan(row *ImpressionCount, dbCols []string) []any {
	targets := make([]any, 0, len(dbCols)+1)
	for _, col := range dbCols {
		switch col {
		case "app_id":
			targets = append(targets, &row.AppID)
		case "app_pubkey":
			targets = append(targets, &row.AppPubkey)
		case "day":
			targets = append(targets, &row.Day)
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

// normalizeDay truncates the day string to 10 characters if it exceeds that length.
// This is done because Sqlite returns the day as a string with the time included,
// e.g. "2023-01-01 12:00:00".
func normalizeDay(day string) string {
	if len(day) > 10 {
		return day[:10]
	}
	return day
}
