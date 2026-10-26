package search

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nbd-wtf/go-nostr"
)

// Entry is one selected listing as search sees it.
type Entry struct {
	App
	Event nostr.Event
}

// Loader returns the selected listings with their artifacts.
type Loader func(ctx context.Context) ([]Entry, error)

// AppLoader returns one app's entry. ok is false when the app is not visible.
type AppLoader func(ctx context.Context, appID string) (Entry, bool, error)

// Encoder turns a normalized query into a leaf-ir-v1 vector.
type Encoder func(ctx context.Context, query string) ([]int8, error)

// Engine ranks the live selection. Load fills it before the relay serves.
// Sync replaces one app after a write. A nil encoder, or a failing query,
// ranks without the vector.
type Engine struct {
	load   Loader
	one    AppLoader
	encode Encoder
	log    *slog.Logger

	mu      sync.Mutex
	entries []Entry
}

func NewEngine(load Loader, one AppLoader, encode Encoder, log *slog.Logger) *Engine {
	return &Engine{load: load, one: one, encode: encode, log: log}
}

// Request is one search. Match, when set, must accept a listing's kind 32267
// before it is ranked.
type Request struct {
	Query  string
	Add    []string
	Remove []string
	Match  func(nostr.Event) bool
	Limit  int
}

// Result is the ranked listings.
type Result struct {
	Hits []Entry
}

// The live-read tokens. A search holding one of them and no query text
// returns the selected catalog instead of a ranking.
const (
	ListingAll   = "listing:all"
	ListingAsset = "listing:asset"
)

// LiveRead reports whether search is a live read, and whether it asks for
// `listing:asset` only. Both tokens together answer `listing:all`. Any query
// text left after the extension tokens makes it a search.
func LiveRead(search string) (assetOnly, ok bool) {
	if query, _, _ := Extensions(search); query != "" {
		return false, false
	}
	var all, asset bool
	for _, token := range strings.Fields(strings.ToLower(search)) {
		switch token {
		case ListingAll:
			all = true
		case ListingAsset:
			asset = true
		}
	}
	if all {
		return false, true
	}
	return asset, asset
}

// Extensions takes the NIP-50 `filter:<fact>` and `-filter:<fact>` tokens out
// of a search string. The first adds the fact as a hard filter; the second
// drops it, including one parsed from a phrase. A token naming an unknown
// fact is dropped too, and so are the live-read tokens.
func Extensions(search string) (query string, add, remove []string) {
	var rest []string
	for _, token := range strings.Fields(search) {
		lower := strings.ToLower(token)
		if lower == ListingAll || lower == ListingAsset {
			continue
		}
		fact, negated := strings.CutPrefix(lower, "-")
		fact, ok := strings.CutPrefix(fact, "filter:")
		if !ok {
			rest = append(rest, token)
			continue
		}
		if !slices.Contains(Facts, fact) {
			continue
		}
		if negated {
			remove = append(remove, fact)
		} else {
			add = append(add, fact)
		}
	}
	return strings.Join(rest, " "), add, remove
}

// Search ranks the selected listings for req, as the webapp and Android do.
func (e *Engine) Search(ctx context.Context, req Request) (Result, error) {
	limit := req.Limit
	if limit <= 0 || limit > Limit {
		limit = Limit
	}
	query := Normalize(req.Query)
	if runeLen(query) < MinSearchLength {
		return Result{}, nil
	}
	entries, err := e.snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	rows := entries
	if req.Match != nil {
		rows = make([]Entry, 0, len(entries))
		for _, entry := range entries {
			if req.Match(entry.Event) {
				rows = append(rows, entry)
			}
		}
	}
	named := slices.ContainsFunc(rows, func(row Entry) bool {
		return strings.ToLower(row.ID) == query || Fold(row.Name) == query
	})
	residual := query
	var parsed Parsed
	if !named {
		parsed = Parse(query)
		residual = parsed.Residual
	}
	hard, exclude := 0, 0
	for _, fact := range Facts {
		if (slices.Contains(parsed.Facts, fact) || slices.Contains(req.Add, fact)) && !slices.Contains(req.Remove, fact) {
			hard |= HardBit(fact)
			exclude |= ExcludeBit(fact)
		}
	}
	var vector []int8
	if residual != "" && e.encode != nil {
		vector, err = e.encode(ctx, query)
		if err != nil {
			e.log.Warn("search: encoder failed; ranking without the vector", "error", err)
			vector = nil
		}
	}
	apps := make([]App, len(rows))
	byID := make(map[string]Entry, len(rows))
	for i, row := range rows {
		apps[i] = row.App
		byID[row.ID] = row
	}
	ranked := Rank(apps, residual, vector, hard, limit, exclude, false, hard, exclude)
	hits := make([]Entry, len(ranked))
	for i, app := range ranked {
		hits[i] = byID[app.ID]
	}
	return Result{Hits: hits}, nil
}

// Load reads the selected listings now. On failure the previous selection
// stays.
func (e *Engine) Load(ctx context.Context) error {
	entries, err := e.load(ctx)
	if err != nil {
		return err
	}
	if entries == nil {
		entries = []Entry{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.entries = entries
	return nil
}

// Sync replaces the entry for appID, or drops it when the app is not visible.
// It does nothing before Load.
func (e *Engine) Sync(ctx context.Context, appID string) error {
	if e.one == nil || appID == "" {
		return nil
	}
	e.mu.Lock()
	loaded := e.entries != nil
	e.mu.Unlock()
	if !loaded {
		return nil
	}
	entry, ok, err := e.one(ctx, appID)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make([]Entry, 0, len(e.entries)+1)
	for _, old := range e.entries {
		if old.ID != appID {
			next = append(next, old)
		}
	}
	if ok {
		next = append(next, entry)
	}
	e.entries = next
	return nil
}

// snapshot returns the loaded selection, loading it first when nothing has.
func (e *Engine) snapshot(ctx context.Context) ([]Entry, error) {
	e.mu.Lock()
	entries := e.entries
	e.mu.Unlock()
	if entries != nil {
		return entries, nil
	}
	if err := e.Load(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.entries, nil
}
