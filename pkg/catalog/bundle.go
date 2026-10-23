package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// sealing keeps the interval and a manual seal from writing the same epoch.
var sealing sync.Mutex

// Enricher writes artifacts for the app ids in a bundle diff.
// A cache that already matches the listing does nothing. Nil skips enrichment.
type Enricher func(ctx context.Context, appIDs []string) error

// Bundle writes the next snapshot when the live catalog changed. The snapshot
// is always the whole selected catalog.
// filter is an app-id substring that limits enrichment. Empty enriches the
// apps in the diff. Otherwise enrich runs for the listings whose app id
// contains it, and no other app is enriched. No match is an error. The
// interval loop passes an empty filter.
// Artifact files are included when the enricher cache matches the listing's current APK hash.
func Bundle(ctx context.Context, data, dbPath, filter string, signer Signer, notices map[string]string, enrich Enricher) (int64, error) {
	sealing.Lock()
	defer sealing.Unlock()
	raw, err := LoadEvents(ctx, dbPath, signer.PubKey, signer.PubKey)
	if err != nil {
		return 0, err
	}
	at := time.Now()
	filter = strings.TrimSpace(filter)
	next := ResolveAt(raw, signer.PubKey, at)
	run := enrich
	if filter != "" {
		matched := matchingApps(next, filter)
		if len(matched) == 0 {
			return 0, fmt.Errorf("no listings match %q", filter)
		}
		writeBlock("filter " + filter)
		if enrich != nil {
			if err := enrich(ctx, matched); err != nil {
				writeBlock("enrich " + err.Error())
			}
		}
		run = nil
	}
	next.Notices = notices
	return CreateBundle(ctx, data, next, signer, at.Unix(), run)
}

// matchingApps are the app ids in st that contain filter.
func matchingApps(st State, filter string) []string {
	var out []string
	for _, listing := range st.Listings {
		if strings.Contains(listing.AppID, filter) {
			out = append(out, listing.AppID)
		}
	}
	return out
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
