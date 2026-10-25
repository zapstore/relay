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

	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	catalog "github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/config"
	"github.com/zapstore/relay/pkg/control"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/relay/pkg/search"
	"github.com/zapstore/relay/pkg/vertex"
	"github.com/zapstore/steroid/artifacts"
	"github.com/zapstore/steroid/encode"
	"github.com/zapstore/steroid/enrich"
	"golang.org/x/sync/errgroup"
)

func printHelp() {
	fmt.Fprintf(os.Stderr, `relay version %q
Usage:
  relay <command>

Commands:
  run      Start the relay
  bundle [app-id]  Write one catalog snapshot. With an app id, enrich only listings whose id contains it
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

	dataDir := cfg.Sys.Dir
	if err = os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	relayDBPath := filepath.Join(dataDir, "relay.db")
	relayDB, err := relay.NewDB(relayDBPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, relayDB.Close()) }()
	cfg.Blossom.Dir = filepath.Join(dataDir, "blossom")
	cfg.Relay.APKCacheDir = filepath.Join(dataDir, "apk-cache")

	blossomDB, err := blossom.NewDB(filepath.Join(dataDir, "blossom.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, blossomDB.Close()) }()

	var analyticsDB analytics.DB
	var stats *analytics.Engine
	if cfg.Analytics.Enabled() {
		analyticsDB, err = analytics.NewDB(filepath.Join(dataDir, "analytics.db"))
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, analyticsDB.Close()) }()
	} else {
		slog.Info("ANALYTICS_ADDRESS unset: analytics is off")
	}

	signer, err := catalog.OpenSigner(ctx, cfg.Catalog.SignWith)
	if err != nil {
		return err
	}
	if cfg.Relay.Info.Pubkey == "" {
		cfg.Relay.Info.Pubkey = signer.PubKey
	}
	if signer.PubKey != cfg.Relay.Info.Pubkey {
		return fmt.Errorf("SIGN_WITH public key %s does not match RELAY_PUBKEY", signer.PubKey)
	}

	limiter := rate.NewLimiter(cfg.Limiter)
	pol, err := policy.Open(relayDB.DB)
	if err != nil {
		return err
	}
	ban := func(ctx context.Context, pub policy.Publisher, reason string) error {
		return relay.ApplyBan(ctx, relayDB, pol, cfg.Relay.Info.Pubkey, pub, reason)
	}
	// The gate: the catalog key, a key that already published a kind 32267,
	// a whitelisted key, or a pubkey above the web of trust threshold may publish.
	// An app id that already has a kind 32267 skips the gate.
	// Without VERTEX_SECRET_KEY only those keys, and listings for an app the
	// relay already has, may publish.
	var wot policy.WebOfTrust
	var credits vertex.Client
	watch := false
	if cfg.Vertex.Enabled() {
		credits = vertex.NewClient(cfg.Vertex)
		wot = credits
		watch = true
	} else {
		slog.Info("VERTEX_SECRET_KEY unset: web of trust is off, only the catalog key, whitelisted keys, and keys that already published may publish")
	}
	gate := policy.NewGate(pol, cfg.Relay.Info.Pubkey, wot)

	if cfg.Analytics.Enabled() {
		stats = analytics.NewEngine(cfg.Analytics, analyticsDB)
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
		cfg.Sys.ReadOnly,
	)
	if err != nil {
		return err
	}

	// Every admitted release is enriched just before it is listed. A seal only
	// enriches when an operator names a filter.
	rel.UseProcessor(func(ctx context.Context, listing catalog.Listing, picture string) (string, error) {
		return "", enrich.Listing(ctx, dataDir, listing, picture)
	})
	seal := func(ctx context.Context, req control.SealRequest) (string, error) {
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
		var runEnrich catalog.Enricher
		if req.Filter != "" {
			runEnrich = func(ctx context.Context, _ []string) error {
				return enrich.Filter(ctx, dataDir, relayDBPath, req.Filter)
			}
		}
		before, err := catalog.LatestSnap(dataDir)
		if err != nil {
			return "", err
		}
		epoch, err := catalog.Bundle(ctx, dataDir, relayDBPath, req.Filter, signer, notes, runEnrich)
		if err != nil {
			return "", err
		}
		return sealResult(before, epoch), nil
	}
	enrichOne := func(ctx context.Context, id string) (string, error) {
		ctx, text := enrich.WithReport(ctx)
		err := enrich.IDs(ctx, dataDir, relayDBPath, []string{id})
		return text(), err
	}

	if os.Args[1] == "bundle" {
		filter, err := bundleFilter(os.Args)
		if err != nil {
			return err
		}
		line, err := seal(ctx, control.SealRequest{Filter: filter})
		if err != nil {
			return err
		}
		slog.Info("bundle", "result", line)
		return nil
	}

	socketPath := cfg.Control.Socket
	if socketPath == "" {
		socketPath = filepath.Join(dataDir, "relay.sock")
	}
	admins := cfg.Control.AdminPubkeys
	sock := &control.Server{
		Store:    pol,
		Ban:      ban,
		Seal:     seal,
		Enrich:   control.EnrichFunc(enrichOne),
		Data:     dataDir,
		Relay:    cfg.Relay.Info.Pubkey,
		Admins:   admins,
		ReadOnly: cfg.Sys.ReadOnly,
		Nostr:    rel.ServeNostr,
	}
	bundle := catalog.Handler{Data: dataDir, Signer: signer, Notices: pol.Notices, Log: slog.Default(), Limit: limiter}
	if stats != nil {
		bundle.Count = stats
	}
	enc, err := encode.Open(ctx, enrich.ModelDir(dataDir))
	if err != nil {
		return fmt.Errorf("search encoder %s: %w", enrich.ModelDir(dataDir), err)
	}
	defer func() { err = errors.Join(err, enc.Close()) }()
	engine := search.NewEngine(relay.SearchListings(relayDB, cfg.Relay.Info.Pubkey, artifacts.Path(dataDir)), enc.Query, time.Minute, slog.Default())
	rel.UseSearch(engine)
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
		slog.Info("relay socket", "path", socketPath, "admins", len(admins))
		return sock.Listen(gctx, socketPath)
	})
	if cfg.Catalog.Interval > 0 {
		g.Go(func() error {
			catalog.Loop(gctx, cfg.Catalog.Interval, dataDir, relayDBPath, signer, pol.Notices, slog.Default(), nil)
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

// routes serves GET /v1/bundle beside the WebSocket relay. The path carries
// no method so a wrong method reaches the handler and gets 405. Other /vN
// paths hit the same handler and get 400.
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

// sealResult is the line a seal reports.
func sealResult(before, after int64) string {
	if after == before {
		if after == 0 {
			return "Catalog unchanged"
		}
		return fmt.Sprintf("Catalog unchanged at snapshot %d", after)
	}
	return fmt.Sprintf("Sealed bundle %d-%d", before, after)
}
