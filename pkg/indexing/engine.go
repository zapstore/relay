// Package indexing provides a non-blocking engine for recording demand signals
// from the relay into the shared indexing.db database.
//
// The engine follows the analytics engine pattern: a background goroutine
// batches writes so that request handlers are never blocked.
package indexing

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/zapstore/relay/pkg/indexing/store"
	"github.com/zapstore/relay/pkg/repourl"
)

const (
	defaultQueueSize = 4096
	minTTL           = 1 * time.Hour
	maxTTL           = 7 * 24 * time.Hour
)


type discoveryMsg struct{ url string }
type releaseMsg struct{ appID string }
type releaseResetMsg struct{ appID string }

// Engine records demand signals non-blocking and flushes them to indexing.db.
type Engine struct {
	store    *store.Store
	config   Config
	log      *slog.Logger
	ch       chan any
	wg       sync.WaitGroup
	done     chan struct{}

	releaseMu      sync.Mutex
	releaseQueued  map[string]struct{}
}

// NewEngine opens the indexing store at paths.Store and starts the background engine.
// Returns an error if the store cannot be opened.
func NewEngine(cfg Config, paths Paths, logger *slog.Logger) (*Engine, error) {
	s, err := store.New(paths.Store)
	if err != nil {
		return nil, fmt.Errorf("indexing: open store: %w", err)
	}
	return newEngine(s, cfg, logger), nil
}

func newEngine(s *store.Store, cfg Config, logger *slog.Logger) *Engine {
	e := &Engine{
		store:         s,
		config:        cfg,
		log:           logger,
		ch:            make(chan any, cfg.QueueSize),
		done:          make(chan struct{}),
		releaseQueued: make(map[string]struct{}),
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.run()
	}()
	return e
}

// Close shuts down the background goroutine, waits for it to finish, and closes the store.
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
	e.store.Close()
}

// RecordDiscoveryMiss records a repository URL that returned zero search results.
// The search term is parsed for a /:user/:repo path pattern (any host). Plain-text
// searches that don't resolve to a repo URL are silently dropped.
// Non-blocking: if the channel is full, the write is dropped.
func (e *Engine) RecordDiscoveryMiss(rawSearch string) {
	r, ok := repourl.Parse(rawSearch)
	if !ok {
		return
	}
	select {
	case e.ch <- discoveryMsg{url: r.Canonical}:
	default:
		e.log.Warn("indexing: discovery channel full, dropping", "url", r.Canonical)
	}
}

// RecordReleaseRequest records that a user requested releases for an app.
// Non-blocking: if the channel is full, the write is dropped.
// Duplicate app IDs already in the queue are silently skipped.
func (e *Engine) RecordReleaseRequest(appID string) {
	if appID == "" {
		return
	}
	if rand.IntN(10) != 0 {
		return
	}
	e.releaseMu.Lock()
	if _, queued := e.releaseQueued[appID]; queued {
		e.releaseMu.Unlock()
		return
	}
	e.releaseQueued[appID] = struct{}{}
	e.releaseMu.Unlock()

	select {
	case e.ch <- releaseMsg{appID: appID}:
	default:
		e.releaseMu.Lock()
		delete(e.releaseQueued, appID)
		e.releaseMu.Unlock()
		e.log.Warn("indexing: release request channel full, dropping", "app_id", appID)
	}
}

// ResetReleaseRequest resets the request_count for an app after a release is successfully stored.
// Non-blocking: if the channel is full, the reset is dropped (acceptable — it's best-effort).
func (e *Engine) ResetReleaseRequest(appID string) {
	if appID == "" {
		return
	}
	select {
	case e.ch <- releaseResetMsg{appID: appID}:
	default:
		e.log.Warn("indexing: reset channel full, dropping", "app_id", appID)
	}
}

func (e *Engine) run() {
	for {
		select {
		case <-e.done:
			// Drain remaining messages before exit
			e.drain()
			return

		case msg := <-e.ch:
			e.handle(msg)
		}
	}
}

func (e *Engine) drain() {
	for {
		select {
		case msg := <-e.ch:
			e.handle(msg)
		default:
			return
		}
	}
}

func (e *Engine) handle(msg any) {
	switch m := msg.(type) {
	case discoveryMsg:
		if err := e.store.UpsertDiscovery(m.url); err != nil {
			e.log.Error("indexing: failed to upsert discovery", "url", m.url, "error", err)
		}
	case releaseMsg:
		e.releaseMu.Lock()
		delete(e.releaseQueued, m.appID)
		e.releaseMu.Unlock()
		if err := e.store.UpsertReleaseRequest(m.appID); err != nil {
			e.log.Error("indexing: failed to upsert release request", "app_id", m.appID, "error", err)
		}
	case releaseResetMsg:
		if err := e.store.ResetReleaseRequest(m.appID); err != nil {
			e.log.Error("indexing: failed to reset release request", "app_id", m.appID, "error", err)
		}
	}
}
