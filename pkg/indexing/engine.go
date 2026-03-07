// Package indexing provides a non-blocking engine for recording demand signals
// from the relay into the shared indexing.db database.
//
// The engine follows the analytics engine pattern: a background goroutine
// batches writes so that request handlers are never blocked.
package indexing

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zapstore/relay/pkg/indexing/store"
)

const (
	defaultQueueSize = 512
	minTTL           = 1 * time.Hour
	maxTTL           = 7 * 24 * time.Hour
)

// Config holds configuration for the indexing engine.
type Config struct {
	QueueSize int
	MinTTL    time.Duration
	MaxTTL    time.Duration
}

// NewConfig returns a Config with sensible defaults.
func NewConfig() Config {
	return Config{
		QueueSize: defaultQueueSize,
		MinTTL:    minTTL,
		MaxTTL:    maxTTL,
	}
}

type discoveryMsg struct{ url string }
type releaseMsg struct{ appID string }

// Engine records demand signals non-blocking and flushes them to indexing.db.
type Engine struct {
	store    *store.Store
	config   Config
	log      *slog.Logger
	ch       chan any
	wg       sync.WaitGroup
	done     chan struct{}
}

// New creates and starts an indexing engine backed by the given store.
func New(s *store.Store, cfg Config, logger *slog.Logger) *Engine {
	e := &Engine{
		store:  s,
		config: cfg,
		log:    logger,
		ch:     make(chan any, cfg.QueueSize),
		done:   make(chan struct{}),
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.run()
	}()
	return e
}

// Close shuts down the background goroutine and waits for it to finish.
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
}

// RecordDiscoveryMiss records a GitHub URL that returned zero search results.
// Only URLs with "https://github.com/" prefix are accepted; others are silently dropped.
// Non-blocking: if the channel is full, the write is dropped.
func (e *Engine) RecordDiscoveryMiss(url string) {
	if !strings.HasPrefix(url, "https://github.com/") {
		return
	}
	select {
	case e.ch <- discoveryMsg{url: url}:
	default:
		e.log.Warn("indexing: discovery channel full, dropping", "url", url)
	}
}

// RecordReleaseRequest records that a user requested releases for an app.
// Non-blocking: if the channel is full, the write is dropped.
func (e *Engine) RecordReleaseRequest(appID string) {
	if appID == "" {
		return
	}
	select {
	case e.ch <- releaseMsg{appID: appID}:
	default:
		e.log.Warn("indexing: release request channel full, dropping", "app_id", appID)
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
		if err := e.store.UpsertReleaseRequest(m.appID); err != nil {
			e.log.Error("indexing: failed to upsert release request", "app_id", m.appID, "error", err)
		}
	}
}
