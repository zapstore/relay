package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Enricher writes artifacts for the app ids in a bundle diff.
// A cache that already matches the listing does nothing. Nil skips enrichment.
type Enricher func(ctx context.Context, appIDs []string) error

// Bundle writes the next snapshot when the live catalog changed.
// filter is an app-id substring. Empty keeps the whole catalog. Otherwise
// events that do not belong to a matching listing are dropped, and enrich
// runs for those listings before the snapshot is written. No match is an error.
// The interval loop passes an empty filter.
// Artifact files are included when the enricher cache matches the listing's current APK hash.
func Bundle(ctx context.Context, data, dbPath, filter string, signer Signer, notices map[string]string, enrich Enricher) (int64, error) {
	raw, err := LoadEvents(ctx, dbPath, signer.PubKey, signer.PubKey)
	if err != nil {
		return 0, err
	}
	at := time.Now()
	next := ResolveAt(raw, signer.PubKey, at)
	filter = strings.TrimSpace(filter)
	run := enrich
	if filter != "" {
		next = next.matching(filter)
		if len(next.Listings) == 0 {
			return 0, fmt.Errorf("no listings match %q", filter)
		}
		writeBlock("filter " + filter)
		if enrich != nil {
			if err := enrich(ctx, appIDs(next)); err != nil {
				writeBlock("enrich " + err.Error())
			}
		}
		run = nil
	}
	next.Notices = notices
	return CreateBundle(ctx, data, next, signer, at.Unix(), run)
}

// Loop bundles once, then again every interval, until ctx is cancelled.
func Loop(ctx context.Context, every time.Duration, data, dbPath string, signer Signer, notices func(context.Context) (map[string]string, error), log *slog.Logger, enrich Enricher) {
	if every <= 0 {
		return
	}
	run := func() {
		var notes map[string]string
		if notices != nil {
			var err error
			notes, err = notices(ctx)
			if err != nil {
				log.Error("bundle", "error", err)
				return
			}
		}
		n, err := Bundle(ctx, data, dbPath, "", signer, notes, enrich)
		if err != nil {
			log.Error("bundle", "error", err)
			return
		}
		log.Info("bundle", "epoch", n)
	}
	run()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
