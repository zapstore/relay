package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	catalog "github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/config"
	"github.com/zapstore/relay/pkg/dashboard"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/relay/pkg/vertex"
	"github.com/zapstore/steroid/artifacts"
	"github.com/zapstore/steroid/enrich"
	"golang.org/x/sync/errgroup"
)

func printHelp() {
	fmt.Fprintf(os.Stderr, `relay version %q
Usage:
  relay <command>

Commands:
  run      Start the relay
  bundle [app-id]  Write one catalog snapshot. With an app id, only listings whose id contains it
  version  Print the version
  config   Print the active configuration
`, config.Version)
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}
	if os.Args[1] == "version" {
		fmt.Println(config.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	if err := cfg.Validate(); err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "config":
		fmt.Println(cfg)
	case "run", "bundle":
		if err := start(cfg); err != nil {
			fail(err)
		}
	default:
		printHelp()
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "relay: %v\n", err)
	os.Exit(1)
}

func start(cfg config.Config) (err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.SetDefault(
		slog.New(slog.NewTextHandler(os.Stdout, cfg.Sys.LogOptions())),
	)

	slog.Info(fmt.Sprintf("-------------------relay startup %s-------------------", cfg.Sys.Version))
	defer slog.Info("-------------------relay shutdown-------------------")
	if cfg.Sys.ReadOnly {
		slog.Info("read-only: rejecting events, uploads, and bans")
	}

	dataDir := filepath.Join(cfg.Sys.Dir, "data")
	if err = os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	relayDB, err := relay.NewDB(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, relayDB.Close()) }()
	cfg.Blossom.Dir = filepath.Join(dataDir, "blobs")

	blossomDB, err := blossom.NewDB(filepath.Join(dataDir, "blossom.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, blossomDB.Close()) }()

	var analyticsDB analytics.DB
	var stats *analytics.Engine
	if cfg.Analytics.Enabled() {
		analyticsDir := filepath.Join(cfg.Sys.Dir, "analytics")
		if err = os.MkdirAll(analyticsDir, 0755); err != nil {
			return err
		}
		analyticsDB, err = analytics.NewDB(filepath.Join(analyticsDir, "analytics.db"))
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, analyticsDB.Close()) }()
	} else {
		slog.Info("ANALYTICS_API_ADDRESS unset: analytics is off")
	}

	limiter := rate.NewLimiter(cfg.Limiter)
	pol, err := policy.Open(relayDB.DB)
	if err != nil {
		return err
	}
	ban := func(ctx context.Context, pub policy.Publisher, reason string) error {
		return relay.ApplyBan(ctx, relayDB, pol, pub, reason)
	}
	// The gate: the catalog key, a key that already published a kind 32267,
	// or a pubkey above the web of trust threshold may publish.
	// Without VERTEX_SECRET_KEY only the catalog key and keys that already
	// published may publish.
	var wot policy.WebOfTrust
	var credits vertex.Client
	watch := false
	if cfg.Vertex.Enabled() {
		credits = vertex.NewClient(cfg.Vertex)
		wot = credits
		watch = true
	} else {
		slog.Info("VERTEX_SECRET_KEY unset: web of trust is off, only the catalog key and keys that already published may publish")
	}
	gate := policy.NewGate(pol, cfg.Relay.Info.Pubkey, wot)
	signer, err := catalog.OpenSigner(ctx, cfg.Catalog.SignWith)
	if err != nil {
		return err
	}
	if signer.PubKey != cfg.Relay.Info.Pubkey {
		return fmt.Errorf("SIGN_WITH public key %s does not match RELAY_PUBKEY", signer.PubKey)
	}

	if cfg.Analytics.Enabled() {
		stats, err = analytics.NewEngine(cfg.Analytics, analyticsDB, resolver{db: relayDB})
		if err != nil {
			return err
		}
		defer stats.Close()
	}

	if !cfg.Blossom.Bunny.Configured() {
		slog.Info("Bunny unset: writing blobs locally", "dir", cfg.Blossom.Dir)
	}

	rel, err := relay.Setup(
		cfg.Relay,
		limiter,
		pol,
		gate,
		relayDB,
		blossom.NewBytes(cfg.Blossom, blossomDB),
		stats,
		cfg.Sys.ReadOnly,
	)
	if err != nil {
		return err
	}

	blobs, err := blossom.Setup(
		cfg.Blossom,
		limiter,
		pol,
		gate,
		blossomDB,
		rel,
		stats,
		cfg.Sys.ReadOnly,
	)
	if err != nil {
		return err
	}

	arts, err := artifacts.OpenRead(ctx, artifacts.Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		arts, err = nil, nil
	}
	if err != nil {
		return err
	}
	if arts != nil {
		defer func() { err = errors.Join(err, arts.Close()) }()
	}

	enrichApps := func(ctx context.Context, ids []string) error {
		return enrich.IDs(ctx, dataDir, filepath.Join(dataDir, "relay.db"), ids)
	}
	seal := func(ctx context.Context, req dashboard.SealRequest) (string, error) {
		if req.HasFrom {
			if _, err := catalog.EnsureBundle(ctx, dataDir, req.From, req.To, signer, pol.Notices); err != nil {
				return "", err
			}
			return fmt.Sprintf("Bundle %d-%d is ready", req.From, req.To), nil
		}
		notes, sealErr := notices(ctx, pol)
		if sealErr != nil {
			return "", sealErr
		}
		runEnrich := enrichApps
		if req.Filter != "" {
			runEnrich = func(ctx context.Context, _ []string) error {
				return enrich.Filter(ctx, dataDir, filepath.Join(dataDir, "relay.db"), req.Filter)
			}
		}
		before, err := catalog.LatestSnap(dataDir)
		if err != nil {
			return "", err
		}
		epoch, err := catalog.Bundle(ctx, dataDir, filepath.Join(dataDir, "relay.db"), req.Filter, signer, notes, runEnrich)
		if err != nil {
			return "", err
		}
		return dashboard.SealResult(before, epoch), nil
	}
	enrichOne := func(ctx context.Context, id string) (string, error) {
		ctx, text := enrich.WithReport(ctx)
		err := enrich.IDs(ctx, dataDir, filepath.Join(dataDir, "relay.db"), []string{id})
		return text(), err
	}

	cfg.Dashboard.ReadOnly = cfg.Sys.ReadOnly
	cfg.Dashboard.BundleInterval = cfg.Catalog.Interval
	dash, err := dashboard.New(
		cfg.Dashboard,
		limiter,
		pol,
		ban,
		relayDB,
		blossomDB,
		analyticsDB,
		arts,
		dataDir,
		seal,
		enrichOne,
	)
	if err != nil {
		return err
	}

	if os.Args[1] == "bundle" {
		filter, err := bundleFilter(os.Args)
		if err != nil {
			return err
		}
		line, err := seal(ctx, dashboard.SealRequest{Filter: filter})
		if err != nil {
			return err
		}
		slog.Info("bundle", "result", line)
		return nil
	}

	socketPath := filepath.Join(dataDir, "relay.sock")
	nostrSocket := filepath.Join(dataDir, "nostr.sock")
	bundle := catalog.Handler{Data: dataDir, Signer: signer, Notices: pol.Notices, Log: slog.Default(), Limit: limiter}
	if stats != nil {
		bundle.Count = stats
	}
	mux := routes(bundle.Shared(), rel.Handler())

	g, gctx := errgroup.WithContext(ctx)
	if watch {
		g.Go(func() error {
			credits.WatchCredits(gctx)
			return nil
		})
	}
	g.Go(func() error {
		return rel.StartAndServe(gctx, cfg.Relay.Address, mux)
	})
	g.Go(func() error {
		return blobs.StartAndServe(gctx, cfg.Blossom.Address)
	})
	if stats != nil {
		g.Go(func() error {
			return stats.StartAndServe(gctx, cfg.Analytics.Address)
		})
	}
	g.Go(func() error {
		return dash.StartAndServe(gctx, cfg.Dashboard.Address)
	})
	g.Go(func() error {
		slog.Info("relay socket", "path", socketPath)
		return policy.Listen(gctx, socketPath, pol, ban, cfg.Sys.ReadOnly)
	})
	g.Go(func() error {
		slog.Info("nostr socket", "path", nostrSocket)
		return rel.ListenNostr(gctx, nostrSocket)
	})
	if cfg.Catalog.Interval > 0 {
		g.Go(func() error {
			catalog.Loop(gctx, cfg.Catalog.Interval, dataDir, filepath.Join(dataDir, "relay.db"), signer, pol.Notices, slog.Default(), enrichApps)
			return nil
		})
	}
	if cfg.Blossom.PruneInterval > 0 {
		g.Go(func() error {
			pruneLoop(gctx, cfg.Blossom.PruneInterval, blobs, relayDB.DB)
			return nil
		})
	}
	return g.Wait()
}

// routes serves GET /v1/bundle beside the WebSocket relay.
// The path carries no method so a wrong method reaches its handler and gets 405.
// /v2/bundle and other /vN/bundle paths hit the same handler and get 400.
func routes(bundle, ws http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/{version}/bundle", bundle)
	mux.Handle("/", ws)
	return mux
}

// bundleFilter is the optional app-id substring after `relay bundle`.
func bundleFilter(args []string) (string, error) {
	if len(args) < 3 {
		return "", nil
	}
	if len(args) > 3 {
		return "", fmt.Errorf("usage: relay bundle [app-id]")
	}
	filter := strings.TrimSpace(args[2])
	if filter == "" {
		return "", fmt.Errorf("filter is empty")
	}
	return filter, nil
}

func notices(ctx context.Context, pol *policy.Store) (map[string]string, error) {
	notes, err := pol.Notices(ctx)
	if err != nil {
		return nil, err
	}
	return notes, nil
}

func pruneLoop(ctx context.Context, every time.Duration, blobs *blossom.T, db *sql.DB) {
	run := func() {
		keep, nEvents, err := blossom.ReferencedHashes(ctx, db)
		if err != nil {
			slog.Error("prune", "error", err)
			return
		}
		if nEvents == 0 {
			slog.Warn("prune: event table is empty")
			return
		}
		n, err := blobs.Prune(ctx, keep)
		if err != nil {
			slog.Error("prune", "error", err)
			return
		}
		if n > 0 {
			slog.Info("prune", "blobs", n)
		}
	}
	run()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
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
