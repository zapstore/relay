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
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/config"
	"github.com/zapstore/relay/pkg/dashboard"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/relay/pkg/vertex"
)

func printHelp() {
	fmt.Fprintf(os.Stderr, `relay version %q
Usage:
  relay <command>

Commands:
  run      Start the relay
  version  Print the version
  config   Print the active configuration
`, config.Version)
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	// Step 0.
	// Load and validate configuration from .env
	config, err := config.Load()
	if err != nil {
		panic(err)
	}
	if err := config.Validate(); err != nil {
		panic(err)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(config.Sys.Version)
		os.Exit(0)

	case "config":
		fmt.Println(config)
		os.Exit(0)

	case "run":
		// continues below

	default:
		printHelp()
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.SetDefault(
		slog.New(slog.NewTextHandler(os.Stdout, config.Sys.LogOptions())),
	)

	slog.Info(fmt.Sprintf("-------------------relay startup %s-------------------", config.Sys.Version))
	defer slog.Info("-------------------relay shutdown-------------------")

	// Step 1.
	// Initialize databases and their directories
	dataDir := filepath.Join(config.Sys.Dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		panic(err)
	}
	relayDB, err := relay.NewDB(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		panic(err)
	}
	defer relayDB.Close()
	config.Blossom.Dir = filepath.Join(dataDir, "blobs")

	blossomDB, err := blossom.NewDB(filepath.Join(dataDir, "blossom.db"))
	if err != nil {
		panic(err)
	}
	defer blossomDB.Close()

	analyticsDir := filepath.Join(config.Sys.Dir, "analytics")
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		panic(err)
	}
	analyticsDB, err := analytics.NewDB(filepath.Join(analyticsDir, "analytics.db"))
	if err != nil {
		panic(err)
	}
	defer analyticsDB.Close()

	// Step 2.
	// Rate limiter and the publisher blacklist. The blacklist lives in relay.db.
	limiter := rate.NewLimiter(config.Limiter)
	pol, err := policy.Open(relayDB.DB)
	if err != nil {
		panic(err)
	}
	ban := func(ctx context.Context, pub policy.Publisher, reason string) error {
		return relay.ApplyBan(ctx, relayDB, pol, pub, reason)
	}
	// The gate: the catalog key, a paid publisher, or a pubkey above the
	// web of trust threshold may publish. Without VERTEX_SECRET_KEY only the
	// catalog key and paid publishers may publish.
	var wot policy.WebOfTrust
	if config.Vertex.Enabled() {
		client := vertex.NewClient(config.Vertex)
		wot = client
		go client.WatchCredits(ctx)
	} else {
		slog.Info("VERTEX_SECRET_KEY unset: web of trust is off, only the catalog key and paid publishers may publish")
	}
	gate := policy.NewGate(pol, config.Relay.Info.Pubkey, wot)

	// Step 3.
	// Initialize analytics engine
	analytics, err := analytics.NewEngine(config.Analytics, analyticsDB, resolver{db: relayDB})
	if err != nil {
		panic(err)
	}
	defer analytics.Close()

	// Step 4.
	// Setup relay and blossom server
	if !config.Blossom.Bunny.Configured() {
		slog.Info("Bunny unset: writing blobs locally", "dir", config.Blossom.Dir)
	}

	relay, err := relay.Setup(
		config.Relay,
		limiter,
		pol,
		gate,
		relayDB,
		blossomDB,
		analytics,
	)
	if err != nil {
		panic(err)
	}

	blossom, err := blossom.Setup(
		config.Blossom,
		limiter,
		pol,
		gate,
		ban,
		blossomDB,
		relay,
		analytics,
	)
	if err != nil {
		panic(err)
	}

	// Step 5.
	// Initialize dashboard
	dashboard, err := dashboard.New(
		config.Dashboard,
		limiter,
		pol,
		ban,
		relayDB,
		blossomDB,
		analyticsDB,
	)
	if err != nil {
		panic(err)
	}

	// Step 6.
	// Run everything
	socketPath := filepath.Join(dataDir, "relay.sock")
	exit := make(chan error, 5)
	wg := sync.WaitGroup{}
	wg.Add(5)

	go func() {
		defer wg.Done()
		if err := relay.StartAndServe(ctx, config.Relay.Address); err != nil {
			exit <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := blossom.StartAndServe(ctx, config.Blossom.Address); err != nil {
			exit <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := analytics.StartAndServe(ctx, config.Analytics.Address); err != nil {
			exit <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := dashboard.StartAndServe(ctx, config.Dashboard.Address); err != nil {
			exit <- err
		}
	}()

	go func() {
		defer wg.Done()
		slog.Info("relay socket", "path", socketPath)
		if err := policy.Listen(ctx, socketPath, pol, ban); err != nil && ctx.Err() == nil {
			exit <- err
		}
	}()

	select {
	case <-ctx.Done():
		wg.Wait()
		return

	case err := <-exit:
		panic(err)
	}
}

// Resolver implements [analytics.Resolver]
type resolver struct {
	db relay.DB
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
