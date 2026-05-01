package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/defender/pkg/client"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	blobstore "github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/config"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/indexing"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	eventstore "github.com/zapstore/relay/pkg/relay/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Step 0.
	// Load and validate configuration from .env
	config, err := config.Load()
	if err != nil {
		panic(err)
	}

	if err := config.Validate(); err != nil {
		panic(err)
	}

	logger := slog.Default()
	logger.Info("-------------------server startup-------------------")
	defer logger.Info("-------------------server shutdown-------------------")

	// Step 1.
	// Initialize databases
	dataDir := filepath.Join(config.Sys.Dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		panic(err)
	}

	rstore, err := eventstore.New(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		panic(err)
	}
	defer rstore.Close()

	bstore, err := blobstore.New(filepath.Join(dataDir, "blossom.db"))
	if err != nil {
		panic(err)
	}
	defer bstore.Close()

	// Step 2.
	// Initialize rate limiter and connect to the defender
	limiter := rate.NewLimiter(config.Limiter)

	defender, err := client.Default("localhost:8080")
	if err != nil {
		panic(err)
	}
	if _, err := defender.Health(ctx); err != nil {
		slog.Error("defender health check failed", "error", err)
	}

	// Step 3.
	// Initialize analytics engine
	analyticsDir := filepath.Join(config.Sys.Dir, "analytics")
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		panic(err)
	}

	analytics, err := analytics.NewEngine(
		config.Analytics,
		analytics.Paths{
			Store: filepath.Join(analyticsDir, "analytics.db"),
			Geo:   filepath.Join(analyticsDir, "geo.mmdb"),
		},
		logger,
		resolver{db: rstore},
	)
	if err != nil {
		panic(err)
	}
	defer analytics.Close()

	// Step 4.
	// Initialize indexing engine — indexing.db lives in its own indexing/ folder.
	indexingDir := filepath.Join(config.Sys.Dir, "indexing")
	if err := os.MkdirAll(indexingDir, 0755); err != nil {
		panic(err)
	}

	var indexingEngine *indexing.Engine
	indexingPaths := indexing.Paths{Store: filepath.Join(indexingDir, "indexing.db")}
	indexingEngine, err = indexing.NewEngine(config.Indexing, indexingPaths, logger)
	if err != nil {
		logger.Warn("indexing: failed to open indexing.db, demand-driven features disabled", "error", err)
	} else {
		defer indexingEngine.Close()
		logger.Info("indexing: demand-driven indexing enabled")
	}

	// Step 5.
	// Setup relay and blossom server
	relay, err := relay.Setup(
		config.Relay,
		limiter,
		defender,
		rstore,
		analytics,
		indexingEngine,
	)
	if err != nil {
		panic(err)
	}

	blossom, err := blossom.Setup(
		config.Blossom,
		limiter,
		defender,
		bstore,
		analytics,
		resolver{db: rstore},
	)
	if err != nil {
		panic(err)
	}

	// Step 6.
	// Run everything
	exit := make(chan error, 2)
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		address := "localhost:" + config.Relay.Port
		if err := relay.StartAndServe(ctx, address); err != nil {
			exit <- err
		}
	}()

	go func() {
		defer wg.Done()
		address := "localhost:" + config.Blossom.Port
		if err := blossom.StartAndServe(ctx, address); err != nil {
			exit <- err
		}
	}()

	if config.Analytics.Port != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := "localhost:" + config.Analytics.Port
			logger.Info("analytics: API server starting", "address", address)
			if err := analytics.StartAndServe(ctx, address); err != nil {
				exit <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		wg.Wait()
		return

	case err := <-exit:
		panic(err)
	}
}

// Resolver implements [analytics.Resolver] and [blossom.AssetResolver]
type resolver struct {
	db *sqlite.Store
}

func (r resolver) ResolveAssetURL(ctx context.Context, hash string) (string, bool, error) {
	filter := nostr.Filter{
		Kinds: []int{events.KindAsset},
		Tags:  nostr.TagMap{"x": []string{hash}},
		Limit: 1,
	}
	found, err := r.db.Query(ctx, filter)
	if err != nil || len(found) == 0 {
		return "", false, err
	}
	url, ok := events.Find(found[0].Tags, "url")
	return url, ok, nil
}

func (r resolver) AssetsReferencing(ctx context.Context, hash blossom.Hash) ([]nostr.Event, error) {
	filter := nostr.Filter{
		Kinds: []int{events.KindAsset},
		Tags:  nostr.TagMap{"x": []string{hash.Hex()}},
		Limit: 10,
	}
	assets, err := r.db.Query(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch assets referencing blob %s: %w", hash.Hex(), err)
	}
	return assets, nil
}

func (r resolver) LatestVersion(ctx context.Context, appID, pubkey string) (string, error) {
	filter := nostr.Filter{
		Kinds:   []int{events.KindAsset},
		Authors: []string{pubkey},
		Tags:    nostr.TagMap{"i": []string{appID}},
		Limit:   10,
	}
	assets, err := r.db.Query(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("failed to query: %w", err)
	}

	if len(assets) > 0 {
		version, found := events.Find(assets[0].Tags, "version")
		if !found {
			return "", fmt.Errorf("no version tag found for app %s/%s", appID, pubkey)
		}
		return version, nil

	} else {
		// this is most likely caused by the app being on the old format with kind 1063,
		// so we fall back to the version in the release kind
		aTag := fmt.Sprintf("32267:%s:%s", pubkey, appID)
		filter = nostr.Filter{
			Kinds:   []int{events.KindRelease},
			Authors: []string{pubkey},
			Tags:    nostr.TagMap{"a": []string{aTag}},
			Limit:   1,
		}

		assets, err = r.db.Query(ctx, filter)
		if err != nil {
			return "", fmt.Errorf("failed to query: %w", err)
		}
		if len(assets) == 0 {
			return "", errors.New("no assets found")
		}

		dTag, found := events.Find(assets[0].Tags, "d")
		if !found {
			return "", fmt.Errorf("no d tag found for app %s/%s", appID, pubkey)
		}
		_, version, ok := strings.Cut(dTag, "@")
		if !ok {
			return "", fmt.Errorf("invalid d tag: %s", dTag)
		}
		return version, nil
	}
}
