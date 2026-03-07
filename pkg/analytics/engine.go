// Package analytics provides an analytics [Engine] for collecting privacy-preserving statistics
// useful for Zapstore developers to keep track of app usage.
//
// It also allows for the collection of internal metrics like number of events received.
// Check analytics/store/schema.sql
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	"github.com/pippellia-btc/rely"
	"github.com/zapstore/relay/pkg/analytics/geo"
	"github.com/zapstore/relay/pkg/analytics/store"
)

// Paths holds the file system locations for the analytics engine's files.
type Paths struct {
	Store string
	Geo   string
}

// Engine is the heart of the analytics system. It's responsible for processing data
// and saving it in the database on periodic and bounded batches.
type Engine struct {
	store *store.Store
	geo   *geo.Locator

	downloads          chan store.Download
	impressions        chan store.Impression
	pendingImpressions map[store.Impression]int // Impression --> count
	pendingDownloads   map[store.Download]int   // Download --> count

	relay   relayMetrics
	blossom blossomMetrics

	config Config
	log    *slog.Logger
	wg     sync.WaitGroup
	done   chan struct{}
}

type relayMetrics struct {
	reqs    atomic.Int64
	filters atomic.Int64
	events  atomic.Int64
}

type blossomMetrics struct {
	checks    atomic.Int64
	downloads atomic.Int64
	uploads   atomic.Int64
}

// NewEngine starts the background goroutine and returns the engine.
func NewEngine(c Config, paths Paths, logger *slog.Logger) (*Engine, error) {
	var err error
	engine := &Engine{
		impressions:        make(chan store.Impression, c.QueueSize),
		downloads:          make(chan store.Download, c.QueueSize),
		pendingImpressions: make(map[store.Impression]int),
		pendingDownloads:   make(map[store.Download]int),
		config:             c,
		log:                logger,
		done:               make(chan struct{}),
	}

	engine.store, err = store.New(paths.Store)
	if err != nil {
		return nil, fmt.Errorf("analytics: failed to open database at %q: %w", paths.Store, err)
	}

	if c.GeoEnabled {
		engine.geo, err = geo.NewLocator(c.Geo, paths.Geo)
		if err != nil {
			engine.store.Close()
			return nil, fmt.Errorf("analytics: failed to create geo locator: %w", err)
		}
	}

	engine.wg.Add(1)
	go func() {
		defer engine.wg.Done()
		engine.run()
	}()
	return engine, nil
}

// Close closes the engine.
// It will force [Engine.run] to flush any pending data, close the database connections and return.
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
}

// Drain drains all the Engine's channels on a best effort basis, meaning the first time
// all channels are empty, the function returns.
func (e *Engine) drain() {
	for {
		select {
		case impression := <-e.impressions:
			e.pendingImpressions[impression]++

		case download := <-e.downloads:
			e.pendingDownloads[download]++

		default:
			return
		}
	}
}

// lookupCountry returns the ISO country code for the given IP.
// If geo-location is not enabled or the lookup fails, an empty string is returned.
func (e *Engine) lookupCountry(ip net.IP) string {
	if !e.config.GeoEnabled {
		return ""
	}

	country, err := e.geo.Country(ip)
	if err != nil {
		e.log.Warn("analytics: failed to lookup country", "error", err)
		return ""
	}
	return country
}

// RecordReq records the REQ and the derived impressions.
// The client IP address is only used to lookup the country of the client for analytics purposes.
func (e *Engine) RecordReq(client rely.Client, id string, filters nostr.Filters, events []nostr.Event) {
	e.relay.reqs.Add(1)
	e.relay.filters.Add(int64(len(filters)))

	country := e.lookupCountry(client.IP().Raw)
	impressions := store.NewImpressions(country, id, filters, events)

	for i, impression := range impressions {
		select {
		case e.impressions <- impression:
		default:
			dropped := len(impressions) - i
			e.log.Warn("analytics: failed to record impressions", "error", "channel is full", "dropped", dropped)
			return
		}
	}
}

// RecordEvent records the event.
func (e *Engine) RecordEvent(_ rely.Client, _ *nostr.Event) {
	e.relay.events.Add(1)
}

// RecordCheck records the check.
func (e *Engine) RecordCheck(_ blossy.Request, _ blossom.Hash) {
	e.blossom.checks.Add(1)
}

// RecordDownload records the download of the given hash by the given request.
// The client IP address is only used to lookup the country of the client for analytics purposes.
func (e *Engine) RecordDownload(r blossy.Request, hash blossom.Hash) {
	e.blossom.downloads.Add(1)

	country := e.lookupCountry(r.IP().Raw)
	download := store.NewDownload(country, r.Raw().Header, hash)

	select {
	case e.downloads <- download:
	default:
		e.log.Warn("analytics: failed to record download", "error", "channel is full")
		return
	}
}

// RecordUpload records the upload of a blob with the given upload hints.
func (e *Engine) RecordUpload(_ blossy.Request, _ blossy.UploadHints) {
	e.blossom.uploads.Add(1)
}

func (e *Engine) run() {
	flushTicker := time.NewTicker(e.config.FlushInterval)
	defer flushTicker.Stop()

	var geoTicker <-chan time.Time
	if e.config.GeoEnabled {
		t := time.NewTicker(e.config.GeoRefreshInterval)
		defer t.Stop()
		geoTicker = t.C
	}

	for {
		select {
		case <-e.done:
			e.drain()
			e.log.Info("analytics: flushing all pending data...")
			if err := e.flushAll(); err != nil {
				e.log.Error("analytics: failed to flush", "err", err)
			}

			if err := e.store.Close(); err != nil {
				e.log.Error("analytics: failed to close database", "err", err)
			}
			if e.config.GeoEnabled {
				if err := e.geo.Close(); err != nil {
					e.log.Error("analytics: failed to close geolocation db", "err", err)
				}
			}
			return

		case <-geoTicker:
			e.log.Info("analytics: refreshing geolocation database")
			if err := e.geo.Refresh(context.Background()); err != nil {
				e.log.Error("analytics: failed to refresh geolocation database", "err", err)
			}

		case <-flushTicker.C:
			e.log.Debug("analytics: flushing on interval")
			e.drain()

			if err := e.flushAll(); err != nil {
				e.log.Error("analytics: failed to flush", "err", err)
			}

		case impression := <-e.impressions:
			e.log.Debug("analytics: received impression")
			e.pendingImpressions[impression]++

			if len(e.pendingImpressions) >= e.config.FlushSize {
				if err := e.flushImpressions(); err != nil {
					e.log.Error("analytics: failed to flush impressions", "err", err)
				}
			}

		case download := <-e.downloads:
			e.log.Debug("analytics: received download")
			e.pendingDownloads[download]++

			if len(e.pendingDownloads) >= e.config.FlushSize {
				if err := e.flushDownloads(); err != nil {
					e.log.Error("analytics: failed to flush downloads", "err", err)
				}
			}
		}
	}
}

// pending returns the total number of pending impressions and downloads.
func (e *Engine) pending() int {
	return len(e.pendingImpressions) + len(e.pendingDownloads)
}

// flushAll commits any pending data to the database.
func (e *Engine) flushAll() error {
	for e.pending() > 0 {
		if err := e.flushImpressions(); err != nil {
			return fmt.Errorf("failed to flush impressions: %w", err)
		}

		if err := e.flushDownloads(); err != nil {
			return fmt.Errorf("failed to flush downloads: %w", err)
		}
	}

	if err := e.flushRelayMetrics(); err != nil {
		return fmt.Errorf("failed to flush relay metrics: %w", err)
	}
	if err := e.flushBlossomMetrics(); err != nil {
		return fmt.Errorf("failed to flush blossom metrics: %w", err)
	}
	return nil
}

// flushImpressions commits up to [Config.FlushSize] impressions to the database.
// The operation is guaranteed to terminate within [Config.FlushTimeout].
func (e *Engine) flushImpressions() error {
	if len(e.pendingImpressions) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushTimeout)
	defer cancel()

	flushed := make([]store.ImpressionCount, 0, e.config.FlushSize)
	for impression, count := range e.pendingImpressions {
		if len(flushed) >= e.config.FlushSize {
			break
		}

		if count <= 0 {
			continue
		}

		flushed = append(flushed, store.ImpressionCount{
			Impression: impression,
			Count:      count,
		})
	}

	if err := e.store.SaveImpressions(ctx, flushed); err != nil {
		return fmt.Errorf("failed to save impressions: %w", err)
	}

	for _, f := range flushed {
		delete(e.pendingImpressions, f.Impression)
	}
	return nil
}

// flushDownloads commits up to [Config.FlushSize] downloads to the database.
// The operation is guaranteed to terminate within [Config.FlushTimeout].
func (e *Engine) flushDownloads() error {
	if len(e.pendingDownloads) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushTimeout)
	defer cancel()

	flushed := make([]store.DownloadCount, 0, e.config.FlushSize)
	for download, count := range e.pendingDownloads {
		if len(flushed) >= e.config.FlushSize {
			break
		}

		if count <= 0 {
			continue
		}

		flushed = append(flushed, store.DownloadCount{
			Download: download,
			Count:    count,
		})
	}

	if err := e.store.SaveDownloads(ctx, flushed); err != nil {
		return fmt.Errorf("failed to save downloads: %w", err)
	}

	for _, f := range flushed {
		delete(e.pendingDownloads, f.Download)
	}
	return nil
}

// flushRelayMetrics flushes relay metrics to the database.
func (e *Engine) flushRelayMetrics() error {
	// For the sake of simplicity, metrics are always attributed to the current day.
	// This will cause improper attribution around midnight, up to e.config.FlushInterval.
	// If that's short (e.g. 5 minutes), the error will be really small overall.
	metrics := store.RelayMetrics{
		Day:     store.Today(),
		Reqs:    e.relay.reqs.Swap(0),
		Filters: e.relay.filters.Swap(0),
		Events:  e.relay.events.Swap(0),
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushTimeout)
	defer cancel()

	if err := e.store.SaveRelayMetrics(ctx, metrics); err != nil {
		return fmt.Errorf("failed to save relay metrics: %w", err)
	}
	return nil
}

// flushBlossomMetrics flushes blossom metrics to the database.
func (e *Engine) flushBlossomMetrics() error {
	// For the sake of simplicity, metrics are always attributed to the current day.
	// This will cause improper attribution around midnight, up to e.config.FlushInterval.
	// If that's short (e.g. 5 minutes), the error will be really small overall.
	metrics := store.BlossomMetrics{
		Day:       store.Today(),
		Checks:    e.blossom.checks.Swap(0),
		Downloads: e.blossom.downloads.Swap(0),
		Uploads:   e.blossom.uploads.Swap(0),
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushTimeout)
	defer cancel()

	if err := e.store.SaveBlossomMetrics(ctx, metrics); err != nil {
		return fmt.Errorf("failed to save blossom metrics: %w", err)
	}
	return nil
}
