// Package search provides semantic search for Nostr app events (kind 32267) backed by
// Typesense (keyword + vector, rank-fused). The relay routes all NIP-50 app searches
// here when Typesense is enabled; SQLite FTS5 is only used if Typesense is unreachable.
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/typesense/typesense-go/v3/typesense"
	"github.com/typesense/typesense-go/v3/typesense/api"
	"github.com/typesense/typesense-go/v3/typesense/api/pointer"
)

const (
	collectionName  = "apps"
	workerBufSize   = 256
	backfillBatch   = 100
	backfillLogEvery = 500
)

// appDocument is the shape stored in Typesense.
// Only searchable fields are kept here; full event data stays in SQLite.
type appDocument struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Content string `json:"content,omitempty"`
}

type indexOp struct {
	event   *nostr.Event // non-nil → upsert
	eventID string       // non-empty, event nil → delete
}

// Engine manages the Typesense connection and async indexing worker.
type Engine struct {
	client *typesense.Client
	db     *sql.DB
	ops    chan indexOp
	done   chan struct{}
}

// New creates an Engine, ensures the Typesense collection exists, starts the
// async worker, and launches a background backfill of existing kind 32267 events.
// db must be the relay SQLite *sql.DB (Store.DB).
func New(cfg Config, db *sql.DB) (*Engine, error) {
	client := typesense.NewClient(
		typesense.WithServer(cfg.URL),
		typesense.WithAPIKey(cfg.APIKey),
		typesense.WithConnectionTimeout(5*time.Second),
		typesense.WithNumRetries(2),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureCollection(ctx, client); err != nil {
		return nil, fmt.Errorf("search: ensure collection: %w", err)
	}

	e := &Engine{
		client: client,
		db:     db,
		ops:    make(chan indexOp, workerBufSize),
		done:   make(chan struct{}),
	}

	go e.worker()
	go e.backfill()

	return e, nil
}

// Index enqueues an upsert for a kind 32267 event. Non-blocking: drops silently if full.
func (e *Engine) Index(event *nostr.Event) {
	select {
	case e.ops <- indexOp{event: event}:
	default:
		slog.Warn("search: index queue full, dropping event", "event_id", event.ID)
	}
}

// Delete enqueues a deletion by event ID. Non-blocking: drops silently if full.
func (e *Engine) Delete(eventID string) {
	select {
	case e.ops <- indexOp{eventID: eventID}:
	default:
		slog.Warn("search: index queue full, dropping delete", "event_id", eventID)
	}
}

// Search executes a hybrid keyword+semantic query and returns matching event IDs
// in relevance order. Returns an error if Typesense is unavailable — callers
// should fall back to FTS5 on error.
func (e *Engine) Search(ctx context.Context, filter nostr.Filter) ([]string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	params := &api.SearchCollectionParams{
		Q:             pointer.Any(filter.Search),
		QueryBy:       pointer.Any("name,summary,content,embedding"),
		VectorQuery:   pointer.Any("embedding:([], alpha: 0.5)"),
		ExcludeFields: pointer.Any("embedding"),
		PerPage:       pointer.Any(limit),
	}

	if fb := buildFilterBy(filter); fb != "" {
		params.FilterBy = pointer.Any(fb)
	}

	result, err := e.client.Collection(collectionName).Documents().Search(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("search: typesense query: %w", err)
	}

	if result.Hits == nil {
		return nil, nil
	}

	ids := make([]string, 0, len(*result.Hits))
	for _, hit := range *result.Hits {
		if hit.Document == nil {
			continue
		}
		if id, ok := (*hit.Document)["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Close drains the worker and shuts down the engine.
func (e *Engine) Close() {
	close(e.ops)
	<-e.done
}

// worker drains the ops channel and applies each operation to Typesense.
func (e *Engine) worker() {
	defer close(e.done)
	ctx := context.Background()

	for op := range e.ops {
		if op.event != nil {
			if err := e.upsert(ctx, op.event); err != nil {
				slog.Warn("search: upsert failed", "event_id", op.event.ID, "error", err)
			}
		} else {
			if err := e.deleteDoc(ctx, op.eventID); err != nil {
				slog.Warn("search: delete failed", "event_id", op.eventID, "error", err)
			}
		}
	}
}

func (e *Engine) upsert(ctx context.Context, event *nostr.Event) error {
	doc := eventToDoc(event)
	_, err := e.client.Collection(collectionName).Documents().Upsert(ctx, doc, &api.DocumentIndexParameters{})
	return err
}

func (e *Engine) deleteDoc(ctx context.Context, eventID string) error {
	_, err := e.client.Collection(collectionName).Document(eventID).Delete(ctx)
	return err
}

// backfill pages through all kind 32267 events in SQLite and bulk-upserts them
// into Typesense. Runs once at startup in a goroutine. Safe to re-run (upsert is idempotent).
func (e *Engine) backfill() {
	ctx := context.Background()
	offset := 0
	total := 0

	slog.Info("search: backfill starting")

	for {
		rows, err := e.db.QueryContext(ctx,
			`SELECT id, tags, content FROM events WHERE kind = 32267
			 ORDER BY created_at ASC LIMIT ? OFFSET ?`,
			backfillBatch, offset,
		)
		if err != nil {
			slog.Error("search: backfill query failed", "error", err)
			return
		}

		var batch []interface{}
		for rows.Next() {
			var id, tagsJSON, content string
			if err := rows.Scan(&id, &tagsJSON, &content); err != nil {
				slog.Warn("search: backfill scan error", "error", err)
				continue
			}

			var tags nostr.Tags
			if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
				slog.Warn("search: backfill tags parse error", "event_id", id, "error", err)
				continue
			}

			doc := appDocument{
				ID:      id,
				Content: content,
			}
			for _, tag := range tags {
				if len(tag) < 2 {
					continue
				}
				switch tag[0] {
				case "name":
					doc.Name = tag[1]
				case "summary":
					doc.Summary = tag[1]
				}
			}
			batch = append(batch, doc)
		}
		rows.Close()

		if len(batch) == 0 {
			break
		}

		action := api.Upsert
		_, err = e.client.Collection(collectionName).Documents().Import(ctx, batch, &api.ImportDocumentsParams{
			Action: &action,
		})
		if err != nil {
			slog.Error("search: backfill import failed", "offset", offset, "error", err)
			return
		}

		total += len(batch)
		offset += len(batch)

		if total%backfillLogEvery == 0 {
			slog.Info("search: backfill progress", "indexed", total)
		}

		if len(batch) < backfillBatch {
			break
		}
	}

	slog.Info("search: backfill complete", "total", total)
}

// buildFilterBy translates nostr.Filter fields into a Typesense filter_by expression.
// Platform (filter.Tags["f"]) is intentionally excluded — it stays in SQLite.
func buildFilterBy(filter nostr.Filter) string {
	var parts []string

	if len(filter.Authors) > 0 {
		quoted := make([]string, len(filter.Authors))
		for i, pk := range filter.Authors {
			quoted[i] = pk
		}
		parts = append(parts, "pubkey:["+strings.Join(quoted, ",")+"]")
	}

	if filter.Since != nil {
		parts = append(parts, fmt.Sprintf("created_at:>=%d", filter.Since.Time().Unix()))
	}

	if filter.Until != nil {
		parts = append(parts, fmt.Sprintf("created_at:<=%d", filter.Until.Time().Unix()))
	}

	if vals, ok := filter.Tags["t"]; ok && len(vals) > 0 {
		parts = append(parts, "tags:["+strings.Join(vals, ",")+"]")
	}

	return strings.Join(parts, " && ")
}

// ensureCollection creates the apps collection if it doesn't already exist.
func ensureCollection(ctx context.Context, client *typesense.Client) error {
	_, err := client.Collection(collectionName).Retrieve(ctx)
	if err == nil {
		return nil // already exists
	}

	tokenSeparators := []string{"-", "_", "."}
	schema := &api.CollectionSchema{
		Name:            collectionName,
		TokenSeparators: &tokenSeparators,
		Fields: []api.Field{
			{Name: "id", Type: "string"},
			{Name: "name", Type: "string"},
			{Name: "summary", Type: "string", Optional: pointer.Any(true)},
			{Name: "content", Type: "string", Optional: pointer.Any(true)},
			{Name: "pubkey", Type: "string", Optional: pointer.Any(true)},
			{Name: "created_at", Type: "int64", Optional: pointer.Any(true)},
			{Name: "tags", Type: "string[]", Optional: pointer.Any(true)},
			{
				Name:   "embedding",
				Type:   "float[]",
				NumDim: pointer.Any(384),
				Embed: &struct {
					From        []string `json:"from"`
					ModelConfig struct {
						AccessToken    *string `json:"access_token,omitempty"`
						ApiKey         *string `json:"api_key,omitempty"`
						ClientId       *string `json:"client_id,omitempty"`
						ClientSecret   *string `json:"client_secret,omitempty"`
						IndexingPrefix *string `json:"indexing_prefix,omitempty"`
						ModelName      string  `json:"model_name"`
						ProjectId      *string `json:"project_id,omitempty"`
						QueryPrefix    *string `json:"query_prefix,omitempty"`
						RefreshToken   *string `json:"refresh_token,omitempty"`
						Url            *string `json:"url,omitempty"`
					} `json:"model_config"`
				}{
					From: []string{"name", "summary", "content"},
					ModelConfig: struct {
						AccessToken    *string `json:"access_token,omitempty"`
						ApiKey         *string `json:"api_key,omitempty"`
						ClientId       *string `json:"client_id,omitempty"`
						ClientSecret   *string `json:"client_secret,omitempty"`
						IndexingPrefix *string `json:"indexing_prefix,omitempty"`
						ModelName      string  `json:"model_name"`
						ProjectId      *string `json:"project_id,omitempty"`
						QueryPrefix    *string `json:"query_prefix,omitempty"`
						RefreshToken   *string `json:"refresh_token,omitempty"`
						Url            *string `json:"url,omitempty"`
					}{
						ModelName: "ts/all-MiniLM-L12-v2",
					},
				},
			},
		},
	}

	_, err = client.Collections().Create(ctx, schema)
	return err
}

// eventToDoc converts a kind 32267 nostr.Event to an appDocument for Typesense.
func eventToDoc(event *nostr.Event) appDocument {
	doc := appDocument{
		ID:      event.ID,
		Content: event.Content,
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "name":
			doc.Name = tag[1]
		case "summary":
			doc.Summary = tag[1]
		}
	}
	return doc
}
