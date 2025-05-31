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
	"github.com/nbd-wtf/go-nostr"
)

type SQLite3Backend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (b *SQLite3Backend) Close() {
	b.DB.Close()
}

var ErrDupEvent = errors.New("duplicate: event already exists")

const (
	queryLimit        = 100
	queryIDsLimit     = 500
	queryAuthorsLimit = 500
	queryKindsLimit   = 10
	queryTagsLimit    = 10
)

var ddls = []string{
	`CREATE TABLE IF NOT EXISTS event (
       id text NOT NULL,
       pubkey text NOT NULL,
       created_at integer NOT NULL,
       kind integer NOT NULL,
       tags jsonb NOT NULL,
       content text NOT NULL,
       sig text NOT NULL);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ididx ON event(id)`,
	`CREATE INDEX IF NOT EXISTS pubkeyprefix ON event(pubkey)`,
	`CREATE INDEX IF NOT EXISTS timeidx ON event(created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS kindidx ON event(kind)`,
	`CREATE INDEX IF NOT EXISTS kindtimeidx ON event(kind,created_at DESC)`,
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

	if b.QueryLimit == 0 {
		b.QueryLimit = queryLimit
	}
	if b.QueryIDsLimit == 0 {
		b.QueryIDsLimit = queryIDsLimit
	}
	if b.QueryAuthorsLimit == 0 {
		b.QueryAuthorsLimit = queryAuthorsLimit
	}
	if b.QueryKindsLimit == 0 {
		b.QueryKindsLimit = queryKindsLimit
	}
	if b.QueryTagsLimit == 0 {
		b.QueryTagsLimit = queryTagsLimit
	}
	return nil
}

func (b SQLite3Backend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	_, err := b.DB.ExecContext(ctx, "DELETE FROM event WHERE id = $1", evt.ID)
	return err
}


func (b *SQLite3Backend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	// insert
	tagsj, _ := json.Marshal(evt.Tags)
	res, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO event (id, pubkey, created_at, kind, tags, content, sig)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
    `, evt.ID, evt.PubKey, evt.CreatedAt, evt.Kind, tagsj, evt.Content, evt.Sig)
	if err != nil {
		return err
	}

	nr, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if nr == 0 {
		return ErrDupEvent
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
	query, params, err := b.queryEventsSql(filter, false)
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

var (
	TooManyIDs       = errors.New("too many ids")
	TooManyAuthors   = errors.New("too many authors")
	TooManyKinds     = errors.New("too many kinds")
	TooManyTagValues = errors.New("too many tag values")
	EmptyTagSet      = errors.New("empty tag set")
)

func makePlaceHolders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func (b SQLite3Backend) queryEventsSql(filter nostr.Filter, doCount bool) (string, []any, error) {
	conditions := make([]string, 0, 7)
	params := make([]any, 0, 20)

	if len(filter.IDs) > 0 {
		if len(filter.IDs) > 500 {
			// too many ids, fail everything
			return "", nil, TooManyIDs
		}

		for _, v := range filter.IDs {
			params = append(params, v)
		}
		conditions = append(conditions, `id IN (`+makePlaceHolders(len(filter.IDs))+`)`)
	}

	if len(filter.Authors) > 0 {
		if len(filter.Authors) > b.QueryAuthorsLimit {
			// too many authors, fail everything
			return "", nil, TooManyAuthors
		}

		for _, v := range filter.Authors {
			params = append(params, v)
		}
		conditions = append(conditions, `pubkey IN (`+makePlaceHolders(len(filter.Authors))+`)`)
	}

	if len(filter.Kinds) > 0 {
		if len(filter.Kinds) > 10 {
			// too many kinds, fail everything
			return "", nil, TooManyKinds
		}

		for _, v := range filter.Kinds {
			params = append(params, v)
		}
		conditions = append(conditions, `kind IN (`+makePlaceHolders(len(filter.Kinds))+`)`)
	}

	// tags
	totalTags := 0
	// we use a very bad implementation in which we only check the tag values and ignore the tag names
	for _, values := range filter.Tags {
		if len(values) == 0 {
			// any tag set to [] is wrong
			return "", nil, EmptyTagSet
		}

		orTag := make([]string, len(values))
		for i, tagValue := range values {
			orTag[i] = `tags LIKE ? ESCAPE '\'`
			params = append(params, `%`+strings.ReplaceAll(tagValue, `%`, `\%`)+`%`)
		}

		// each separate tag key is an independent condition
		conditions = append(conditions, "("+strings.Join(orTag, "OR ")+")")

		totalTags += len(values)
		if totalTags > b.QueryTagsLimit {
			// too many tags, fail everything
			return "", nil, TooManyTagValues
		}
	}

	if filter.Since != nil {
		conditions = append(conditions, `created_at >= ?`)
		params = append(params, filter.Since)
	}
	if filter.Until != nil {
		conditions = append(conditions, `created_at <= ?`)
		params = append(params, filter.Until)
	}
	if filter.Search != "" {
		conditions = append(conditions, `content LIKE ? ESCAPE '\'`)
		params = append(params, `%`+strings.ReplaceAll(filter.Search, `%`, `\%`)+`%`)
	}

	if len(conditions) == 0 {
		// fallback
		conditions = append(conditions, `true`)
	}

	if filter.Limit < 1 || filter.Limit > b.QueryLimit {
		params = append(params, b.QueryLimit)
	} else {
		params = append(params, filter.Limit)
	}

	var query string
	if doCount {
		query = sqlx.Rebind(sqlx.BindType("sqlite3"), `SELECT
          COUNT(*)
        FROM event WHERE `+
			strings.Join(conditions, " AND ")+
			" LIMIT ?")
	} else {
		query = sqlx.Rebind(sqlx.BindType("sqlite3"), `SELECT
          id, pubkey, created_at, kind, tags, content, sig
        FROM event WHERE `+
			strings.Join(conditions, " AND ")+
			" ORDER BY created_at DESC, id LIMIT ?")
	}

	return query, params, nil
}
