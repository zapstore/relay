package store

import (
	"context"
	"fmt"
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

// Type represents the type of impression, which is determined by the REQ.
type Type string

const TypeDetail Type = "detail"

// Impression of an app.
type Impression struct {
	AppID       string
	AppPubkey   string
	Day         string // formatted as "YYYY-MM-DD"
	Source      Source
	Type        Type
	CountryCode string // ISO 2 letter code
}

// ImpressionCount is an Impression paired with its occurrence count.
type ImpressionCount struct {
	Impression
	Count int
}

// ImpressionSource returns the Source derived from the REQ subscription id.
// Only the specific detail-screen prefixes used by zapstore ("app-detail-")
// and webapp ("web-app-detail-") are recognized. The second return value
// is false for everything else.
func ImpressionSource(id string) (Source, bool) {
	switch {
	case strings.HasPrefix(id, "app-detail-"):
		return SourceApp, true
	case strings.HasPrefix(id, "web-app-detail-"):
		return SourceWeb, true
	default:
		return SourceUnknown, false
	}
}

// IsDetailFilter reports whether the filter represents an app detail view
// (kind 32267 + author pubkey + d tag).
func IsDetailFilter(filter nostr.Filter) bool {
	for _, k := range filter.Kinds {
		if k == eventPkg.KindApp && len(filter.Authors) > 0 && len(filter.Tags["d"]) > 0 {
			return true
		}
	}
	return false
}

// NewImpressions creates impressions from an app detail REQ.
// Only known sources (app, web) and detail filters (kind 32267 + d tag) produce impressions.
func NewImpressions(country string, id string, filters nostr.Filters, events []nostr.Event) []Impression {
	source, known := ImpressionSource(id)
	if !known {
		return nil
	}

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
				Type:        TypeDetail,
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
