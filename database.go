package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/reflectx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
)

type SQLite3Backend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL string
}

func (b *SQLite3Backend) Close() {
	b.DB.Close()
}

var ErrDupEvent = errors.New("duplicate: event already exists")

var ddls = []string{
	`PRAGMA journal_mode = WAL;`,

	// Main basic event table to keep actual events
	`CREATE TABLE IF NOT EXISTS events (
		id TEXT NOT NULL PRIMARY KEY,
		pubkey TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		kind INTEGER NOT NULL,
		tags JSON NOT NULL,
		content TEXT NOT NULL,
		sig TEXT NOT NULL
	);`,

	// Index table for single-letter tags
	`CREATE TABLE IF NOT EXISTS tags_index(
    fid INTEGER NOT NULL, 
    value TEXT NOT NULL,
    FOREIGN KEY(fid) REFERENCES events(rowid) ON DELETE CASCADE
  	);`,

	// Creating indexes
	`CREATE INDEX IF NOT EXISTS idx_event_pubkey ON events(pubkey);`,
	`CREATE INDEX IF NOT EXISTS idx_event_time ON events(created_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_event_kind ON events(kind);`,
	`CREATE INDEX IF NOT EXISTS value_idx ON tags_index(value);`,

	// Event FTS for NIP-50 search
	`CREATE VIRTUAL TABLE IF NOT EXISTS events_fts
	USING fts5(text,
	          content='',
	          tokenize = 'trigram',
	          contentless_delete = 1
	);`,

	// Indexing incoming events (content, tags) for NIP-50 search
	`CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
    INSERT INTO events_fts (rowid, text)
      SELECT new.rowid, new.content as text
        WHERE new.kind = 1063 OR new.kind = 32267;
    INSERT INTO events_fts (rowid, text)
      SELECT new.rowid, GROUP_CONCAT(json_extract(value, '$[1]'), ' ') as text
        FROM json_each(new.tags)
        WHERE json_extract(value, '$[0]') IN ('url', 'title', 'description', 'name', 'summary', 'alt', 't', 'd', 'f');
    INSERT INTO tags_index (fid, value)
      SELECT new.rowid, json_extract(value, '$[0]') || ':' || json_extract(value, '$[1]')
        FROM json_each(new.tags)
        WHERE LENGTH(json_extract(value, '$[0]')) = 1 OR json_extract(value, '$[0]') IN ('repository', 'url', 'version');
  	END;`,

	`CREATE TRIGGER IF NOT EXISTS event_ad AFTER DELETE ON events BEGIN
    DELETE FROM events_fts WHERE rowid = old.rowid;
    DELETE FROM tags_index WHERE fid = old.rowid;
  END;`,

	// Black list and logs table: not related to Nostr specs
	`CREATE TABLE IF NOT EXISTS blacklist (
       pubkey text NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS logs (
       log text NOT NULL);`,
}

func (b *SQLite3Backend) Init() error {
	db, err := sqlx.Connect("sqlite3", b.DatabaseURL)
	if err != nil {
		return err
	}

	db.Mapper = reflectx.NewMapperFunc("json", sqlx.NameMapper)
	b.DB = db

	for _, ddl := range ddls {
		_, err = b.DB.Exec(ddl)
		if err != nil {
			return err
		}
	}

	return nil
}

func (b SQLite3Backend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	if _, err := b.DB.ExecContext(ctx, `DELETE FROM events WHERE id = ?`, evt.ID); err != nil {
		return err
	}
	return nil
}

func (b *SQLite3Backend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	tagsj, _ := json.Marshal(evt.Tags)
	res, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO events (id, pubkey, created_at, kind, tags, content, sig)
        VALUES (?, ?, ?, ?, ?, ?, ?);
    `, evt.ID, evt.PubKey, evt.CreatedAt, evt.Kind, tagsj, evt.Content, evt.Sig)
	if err != nil {
		return err
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrDupEvent
	}

	return nil
}

func (b *SQLite3Backend) Savelog(ctx context.Context, log string) error {
	_, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO logs (log)
        VALUES ($1)
    `, log)
	if err != nil {
		return err
	}

	return nil
}

func (b *SQLite3Backend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	b.Lock()
	defer b.Unlock()

	filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
	if nostr.IsAddressableKind(evt.Kind) {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	ch, err := b.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to query before replacing: %w", err)
	}

	shouldStore := true
	for previous := range ch {
		if IsOlder(previous, evt) {
			if err := b.DeleteEvent(ctx, previous); err != nil {
				return fmt.Errorf("failed to delete event for replacing: %w", err)
			}
		} else {
			shouldStore = false
		}
	}

	if shouldStore {
		if err := b.SaveEvent(ctx, evt); err != nil && err != ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	return nil
}

func IsOlder(previous, next *nostr.Event) bool {
	return previous.CreatedAt < next.CreatedAt ||
		(previous.CreatedAt == next.CreatedAt && previous.ID > next.ID)
}

func (b SQLite3Backend) QueryEvents(ctx context.Context, filter nostr.Filter) (ch chan *nostr.Event, err error) {
	query, params, err := b.queryEventsSql(filter)
	if err != nil {
		return nil, err
	}

	rows, err := b.DB.QueryContext(ctx, query, params...)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to fetch events using query %q: %w", query, err)
	}

	ch = make(chan *nostr.Event)
	go func() {
		defer rows.Close()
		defer close(ch)
		for rows.Next() {
			var evt nostr.Event
			var timestamp int64
			err := rows.Scan(&evt.ID, &evt.PubKey, &timestamp,
				&evt.Kind, &evt.Tags, &evt.Content, &evt.Sig)
			if err != nil {
				return
			}
			evt.CreatedAt = nostr.Timestamp(timestamp)
			select {
			case ch <- &evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

func (b SQLite3Backend) queryEventsSql(filter nostr.Filter) (string, []any, error) {
	conditions := []string{}
	params := []any{}

	if len(filter.IDs) > 0 {
		place := makePlaceHolders(len(filter.IDs))
		conditions = append(conditions, "events.id IN ("+place+")")
		for _, id := range filter.IDs {
			params = append(params, id)
		}
	}

	if len(filter.Authors) > 0 {
		place := makePlaceHolders(len(filter.Authors))
		conditions = append(conditions, "events.pubkey IN ("+place+")")
		for _, a := range filter.Authors {
			params = append(params, a)
		}
	}

	if len(filter.Kinds) > 0 {
		place := makePlaceHolders(len(filter.Kinds))
		conditions = append(conditions, "events.kind IN ("+place+")")
		for _, k := range filter.Kinds {
			params = append(params, k)
		}
	}

	// handle single-letter tags (e.g. #e, #p, #d)
	for name, values := range filter.Tags {
		if len(values) == 0 {
			return "", nil, errors.New("empty tag set")
		}
		if len(name) != 1 {
			continue
		}

		placeholders := makePlaceHolders(len(values))
		conditions = append(conditions,
			fmt.Sprintf("rowid IN (SELECT fid FROM tags_index WHERE value IN (%s))", placeholders),
		)

		for _, v := range values {
			params = append(params, name+":"+v)
		}
	}

	if filter.Since != nil {
		conditions = append(conditions, "events.created_at >= ?")
		params = append(params, filter.Since)
	}
	if filter.Until != nil {
		conditions = append(conditions, "events.created_at <= ?")
		params = append(params, filter.Until)
	}

	if filter.Search != "" {
		conditions = append(conditions, "events.rowid IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ?)")
		params = append(params, filter.Search)
	}

	if len(conditions) == 0 {
		conditions = append(conditions, "1")
	}

	limitVal := config.DefaultLimit
	if filter.Limit != 0 {
		limitVal = filter.Limit
	}
	params = append(params, limitVal)

	var sqlStr string

	sqlStr = fmt.Sprintf(
		`SELECT id, pubkey, created_at, kind, tags, content, sig
			   FROM events
			  WHERE %s
			  ORDER BY created_at DESC, id
			  LIMIT ?`,
		strings.Join(conditions, " AND "),
	)

	sqlStr = sqlx.Rebind(sqlx.BindType("sqlite3"), sqlStr)
	return sqlStr, params, nil
}

func (b *SQLite3Backend) IsBlacklisted(ctx context.Context, pubkey string) (bool, error) {
	var exists int
	err := b.DB.GetContext(ctx, &exists, `
        SELECT 1 FROM blacklist WHERE pubkey = ?
    `, pubkey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (b *SQLite3Backend) AddToBlacklist(ctx context.Context, pubkey string) error {
	_, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO blacklist (pubkey)
        VALUES (?)
    `, pubkey)
	if err != nil {
		return fmt.Errorf("failed to add pubkey to blacklist: %w", err)
	}

	return nil
}
