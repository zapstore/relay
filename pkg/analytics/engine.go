// Package analytics provides an analytics [Engine] that counts, per day, the
// bundle requests the relay serves, and relay traffic.
// It records no client address or country.
// Check analytics/store/schema.sql
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/analytics/store"
)

// DB is a type alias for the store database.
type DB = *store.T

// NewDB creates a new analytics database at the given path.
func NewDB(path string) (DB, error) {
	return store.New(path)
}

// Engine is the heart of the analytics system. It's responsible for processing data
// and saving it in the database on periodic and bounded batches.
type Engine struct {
	store *store.T

	bundles        chan store.BundleRequest
	pendingBundles map[store.BundleRequest]int // BundleRequest --> count

	relay relayMetrics

	config Config
	wg     sync.WaitGroup
	done   chan struct{}
}

type relayMetrics struct {
	reqs    atomic.Int64
	filters atomic.Int64
	events  atomic.Int64
}

// NewEngine starts the background goroutine and returns the engine.
func NewEngine(c Config, s *store.T) *Engine {
	engine := &Engine{
		store:          s,
		bundles:        make(chan store.BundleRequest, c.QueueSize),
		pendingBundles: make(map[store.BundleRequest]int),
		config:         c,
		done:           make(chan struct{}),
	}

	engine.wg.Add(1)
	go func() {
		defer engine.wg.Done()
		engine.run()
	}()
	return engine
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
		case bundle := <-e.bundles:
			e.pendingBundles[bundle]++

		default:
			return
		}
	}
}

// RecordReq counts a fulfilled REQ and its filters.
func (e *Engine) RecordReq(filters nostr.Filters) {
	e.relay.reqs.Add(1)
	e.relay.filters.Add(int64(len(filters)))
}

// RecordEvent counts a saved or replaced event.
func (e *Engine) RecordEvent() {
	e.relay.events.Add(1)
}

// RecordBundle records one finished GET /v1/bundle response.
// from and to are -1 when that epoch was not established.
// No client address is stored.
func (e *Engine) RecordBundle(from, to int64, status int, protocol, client string) {
	req := store.BundleRequest{
		Day:           store.Today(),
		FromEpoch:     from,
		ToEpoch:       to,
		Status:        status,
		Protocol:      protocol,
		ClientVersion: client,
	}
	select {
	case e.bundles <- req:
	default:
		slog.Warn("analytics: failed to record bundle", "error", "channel is full")
	}
}

func (e *Engine) run() {
	flushTicker := time.NewTicker(e.config.FlushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-e.done:
			e.drain()
			slog.Info("analytics: flushing all pending data...")
			if err := e.flushAll(); err != nil {
				slog.Error("analytics: failed to flush", "err", err)
			}

			if err := e.store.Close(); err != nil {
				slog.Error("analytics: failed to close database", "err", err)
			}
			return

		case <-flushTicker.C:
			slog.Debug("analytics: flushing on interval")
			e.drain()

			if err := e.flushAll(); err != nil {
				slog.Error("analytics: failed to flush", "err", err)
			}

		case bundle := <-e.bundles:
			slog.Debug("analytics: received bundle", "status", bundle.Status, "protocol", bundle.Protocol)
			e.pendingBundles[bundle]++

			if len(e.pendingBundles) >= e.config.FlushSize {
				if err := e.flushBundles(); err != nil {
					slog.Error("analytics: failed to flush bundles", "err", err)
				}
			}
		}
	}
}

// pending returns the number of pending bundle requests.
func (e *Engine) pending() int {
	return len(e.pendingBundles)
}

// flushAll commits any pending data to the database.
func (e *Engine) flushAll() error {
	for e.pending() > 0 {
		if err := e.flushBundles(); err != nil {
			return fmt.Errorf("failed to flush bundles: %w", err)
		}
	}

	if err := e.flushRelayMetrics(); err != nil {
		return fmt.Errorf("failed to flush relay metrics: %w", err)
	}
	return nil
}

// flushBundles commits up to [Config.FlushSize] bundle requests to the database.
// The operation is guaranteed to terminate within [Config.FlushTimeout].
func (e *Engine) flushBundles() error {
	if len(e.pendingBundles) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushTimeout)
	defer cancel()

	flushed := make([]store.BundleCount, 0, e.config.FlushSize)
	for req, count := range e.pendingBundles {
		if len(flushed) >= e.config.FlushSize {
			break
		}
		if count <= 0 {
			continue
		}
		flushed = append(flushed, store.BundleCount{
			BundleRequest: req,
			Count:         count,
		})
	}

	if err := e.store.SaveBundles(ctx, flushed); err != nil {
		return fmt.Errorf("failed to save bundle requests: %w", err)
	}

	for _, f := range flushed {
		delete(e.pendingBundles, f.BundleRequest)
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
