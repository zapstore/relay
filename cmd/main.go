package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/acl"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	blobstore "github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/config"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/indexing"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/relay/pkg/relay/linkverify"
	eventstore "github.com/zapstore/relay/pkg/relay/store"
)

// assetResolver implements blossom.AssetResolver by querying the relay event store
// for kind 3063 (Software Asset) events whose "x" tag matches the requested hash.
type assetResolver struct {
	db *sqlite.Store
}

func (r *assetResolver) ResolveAssetURL(ctx context.Context, hash string) (string, bool, error) {
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

// startBlossom serves handler on addr, gracefully shutting down when ctx is cancelled.
// Timeout values mirror blossy's defaults.
func startBlossom(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	exitErr := make(chan error, 1)
	go func() {
		slog.Info("serving the blossom server", "address", addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exitErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down the blossom server", "address", addr)
		defer slog.Info("blossom server stopped")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-exitErr:
		return err
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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
	// Initialize rate limiter and ACL
	limiter := rate.NewLimiter(config.Limiter)
	aclDir := filepath.Join(config.Sys.Dir, "acl")

	acl, err := acl.New(config.ACL, aclDir, logger)
	if err != nil {
		panic(err)
	}
	defer acl.Close()

	// Step 3.
	// Initialize analytics engine
	analyticsDir := filepath.Join(config.Sys.Dir, "analytics")
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		panic(err)
	}

	paths := analytics.Paths{
		Store: filepath.Join(analyticsDir, "analytics.db"),
		Geo:   filepath.Join(analyticsDir, "geo.mmdb"),
	}

	analytics, err := analytics.NewEngine(config.Analytics, paths, logger)
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
	// Setup C1 verifier and relay/blossom
	c1 := linkverify.New(rstore, acl, config.Blossom.Hostname, logger)

	relay, err := relay.Setup(
		config.Relay,
		limiter,
		acl,
		rstore,
		analytics,
		c1,
		indexingEngine,
	)
	if err != nil {
		panic(err)
	}

	blossomHandler, err := blossom.Setup(
		config.Blossom,
		limiter,
		acl,
		bstore,
		analytics,
		&assetResolver{db: rstore},
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
		if err := startBlossom(ctx, address, blossomHandler); err != nil {
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
