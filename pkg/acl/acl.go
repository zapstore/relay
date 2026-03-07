// The acl package is responsible for managing the access control list for the relay and blossom servers.
// It supports hot-reloadable CSV files for allowed/blocked pubkeys, events, and blobs.
// It also supports a configurable policy for unknown pubkeys (allow, block, or vertex-based reputation filtering).
package acl

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/pippellia-btc/smallset"
	"github.com/zapstore/relay/pkg/acl/repo"
	"github.com/zapstore/relay/pkg/acl/vertex"
	"github.com/zapstore/relay/pkg/events"
)

// Controller is an access control list that manages allowed/blocked pubkeys and platform users.
// It supports hot-reloading of the CSV files when they are modified.
type Controller struct {
	mu                   sync.RWMutex
	pubkeysAllowed       *smallset.Ordered[string]
	pubkeysBlocked       *smallset.Ordered[string]
	platformUsersBlocked *smallset.Ordered[string] // format: "platform:username"

	vertex   vertex.Filter
	verifier *repo.Verifier

	log     *slog.Logger
	watcher *fsnotify.Watcher
	done    chan struct{}

	dir    string
	config Config
}

// New creates a new Controller with the given directory and configuration.
// If the directory or required CSV files don't exist, they are created with default headers.
// It reloads all CSV files when they change, logging using the given logger.
func New(c Config, dir string, log *slog.Logger) (*Controller, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}

	// Resolve absolute path for reliable comparison with fsnotify ids
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve acl directory path: %w", err)
	}
	if err := initDir(absDir); err != nil {
		return nil, fmt.Errorf("failed to initialize acl directory: %w", err)
	}

	acl := &Controller{
		dir:                  absDir,
		config:               c,
		pubkeysAllowed:       smallset.New[string](100),
		pubkeysBlocked:       smallset.New[string](100),
		platformUsersBlocked: smallset.New[string](100),
		log:                  log,
		done:                 make(chan struct{}),
	}

	if c.UnknownPubkeyPolicy == UseVertex {
		acl.vertex = vertex.NewFilter(c.Vertex)
	}

	acl.verifier = repo.NewVerifier(c.Repo)

	if _, err = acl.reloadAllowedPubkeys(); err != nil {
		return nil, fmt.Errorf("failed to load allowed pubkeys: %w", err)
	}
	if _, err = acl.reloadBlockedPubkeys(); err != nil {
		return nil, fmt.Errorf("failed to load blocked pubkeys: %w", err)
	}
	if _, err = acl.reloadBlockedPlatformUsers(); err != nil {
		return nil, fmt.Errorf("failed to load blocked platform users: %w", err)
	}

	acl.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create file watcher: %w", err)
	}

	// We watch the directory instead of individual files because most editors
	// use atomic writes (write to temp file, then rename), which would cause us to lose the watcher.
	if err := acl.watcher.Add(absDir); err != nil {
		acl.watcher.Close()
		return nil, fmt.Errorf("failed to watch acl directory: %w", err)
	}

	go acl.watch()
	return acl, nil
}

// fileTemplates maps each required file to its initial content (header comments).
var fileTemplates = map[string]string{
	PubkeysAllowedFile:       "# Allowed pubkeys\n# pubkey,reason\n",
	PubkeysBlockedFile:       "# Blocked pubkeys\n# pubkey,reason\n",
	PlatformUsersBlockedFile: "# Blocked platform users\n# platform:username,reason\n",
}

// initDir ensures the ACL directory and required files exist.
// Creates the directory and any missing files with default headers.
func initDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %s", dir)
	}

	for file, template := range fileTemplates {
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(path, []byte(template), 0644); err != nil {
				return fmt.Errorf("failed to create %s: %w", file, err)
			}
		}
	}
	return nil
}

// Close stops the file watcher and releases resources.
func (c *Controller) Close() error {
	close(c.done)
	return c.watcher.Close()
}

// AllowPubkey checks if a pubkey should be allowed.
// It checks the blocked list first, then the allowed list, and finally applies the unknown pubkey policy.
func (c *Controller) AllowPubkey(ctx context.Context, pubkey string) (bool, error) {
	c.mu.RLock()
	blocked := c.pubkeysBlocked.Contains(pubkey)
	allowed := c.pubkeysAllowed.Contains(pubkey)
	c.mu.RUnlock()

	if blocked {
		return false, nil
	}

	if allowed {
		return true, nil
	}

	switch c.config.UnknownPubkeyPolicy {
	case AllowAll:
		return true, nil

	case BlockAll:
		return false, nil

	case UseVertex:
		allow, err := c.vertex.Allow(ctx, pubkey)
		if err != nil {
			return false, fmt.Errorf("failed to verify reputation: %w", err)
		}
		return allow, nil

	default:
		return false, fmt.Errorf("internal error: unknown pubkey policy: %s", c.config.UnknownPubkeyPolicy)
	}
}

// AllowEvent checks whether an event's author is allowed to publish.
// For kind 32267 (App) events with a "repository" tag, repo verification is attempted
// before falling through to the configured unknown-pubkey policy.
func (c *Controller) AllowEvent(ctx context.Context, e *nostr.Event) (bool, error) {
	c.mu.RLock()
	blocked := c.pubkeysBlocked.Contains(e.PubKey)
	allowed := c.pubkeysAllowed.Contains(e.PubKey)
	c.mu.RUnlock()

	if blocked {
		return false, nil
	}

	if allowed {
		return true, nil
	}

	// Repo verification: for kind 32267 with a repository tag, check zapstore.yaml
	if e.Kind == events.KindApp {
		if repoURL, ok := events.Find(e.Tags, "repository"); ok && repoURL != "" {
			result, err := c.verifier.Verify(ctx, repoURL, e.PubKey)
			if err != nil {
				c.log.Warn("acl: repo verification failed", "repo", repoURL, "error", err)
				// Fall through to policy — don't hard-fail on network errors
			} else if result.Matched {
				// Check if the platform user is blocked before whitelisting
				if c.IsPlatformUserBlocked(result.Platform, result.Username) {
					c.log.Warn("acl: blocked platform user attempted auto-whitelist",
						"platform", result.Platform, "username", result.Username)
					return false, nil
				}
				reason := fmt.Sprintf("verified via %s repo %s", result.Platform, repoURL)
				if err := c.appendAllowedPubkey(e.PubKey, reason); err != nil {
					return false, fmt.Errorf("failed to append pubkey to allowed list: %w", err)
				}
				c.log.Info("acl: auto-whitelisted via repo verification",
					"pubkey", e.PubKey, "platform", result.Platform, "repo", repoURL)
				return true, nil
			}
		}
	}

	switch c.config.UnknownPubkeyPolicy {
	case AllowAll:
		return true, nil

	case BlockAll:
		return false, nil

	case UseVertex:
		allow, err := c.vertex.Allow(ctx, e.PubKey)
		if err != nil {
			return false, fmt.Errorf("failed to verify reputation: %w", err)
		}
		return allow, nil

	default:
		return false, fmt.Errorf("internal error: unknown pubkey policy: %s", c.config.UnknownPubkeyPolicy)
	}
}

// AppendAllowedPubkey appends a pubkey to the allowed pubkeys CSV file.
// The file watcher will pick up the change and reload the list automatically.
func (c *Controller) AppendAllowedPubkey(pubkey, reason string) error {
	return c.appendAllowedPubkey(pubkey, reason)
}

// appendAllowedPubkey is the internal implementation.
func (c *Controller) appendAllowedPubkey(pubkey, reason string) error {
	csvPath := filepath.Join(c.dir, PubkeysAllowedFile)
	f, err := os.OpenFile(csvPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", PubkeysAllowedFile, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{pubkey, reason}); err != nil {
		return fmt.Errorf("failed to write to %s: %w", PubkeysAllowedFile, err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("failed to flush %s: %w", PubkeysAllowedFile, err)
	}
	return nil
}

// IsPlatformUserBlocked checks if a platform user (format: "platform:username") is blocked.
func (c *Controller) IsPlatformUserBlocked(platform, username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.platformUsersBlocked.Contains(platform + ":" + username)
}

// watch monitors the ACL directory for file changes and reloads the changed file.
func (c *Controller) watch() {
	const delay = 100 * time.Millisecond
	timer := map[string]*time.Timer{}

	for {
		select {
		case <-c.done:
			return

		case event, ok := <-c.watcher.Events:
			if !ok {
				return
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			file := filepath.Base(event.Name)
			if !slices.Contains(RequiredFiles, file) {
				continue
			}

			// debounce the reload by stopping the timer if it exists.
			if timer[file] != nil {
				timer[file].Stop()
			}

			timer[file] = time.AfterFunc(delay, func() {
				count, err := c.reload(file)
				if err != nil {
					c.log.Error("acl: reload failed, using old list", "file", file, "error", err)
					return
				}

				c.log.Info("acl: successful reload", "file", file, "items", count)
			})

		case err, ok := <-c.watcher.Errors:
			if !ok {
				return
			}
			c.log.Error("acl: watcher error", "error", err)
		}
	}
}

// reload reloads the given file.
// It returns the number of entities in the new list.
func (c *Controller) reload(file string) (int, error) {
	switch file {
	case PubkeysAllowedFile:
		return c.reloadAllowedPubkeys()
	case PubkeysBlockedFile:
		return c.reloadBlockedPubkeys()
	case PlatformUsersBlockedFile:
		return c.reloadBlockedPlatformUsers()
	default:
		return 0, fmt.Errorf("unknown file: %s", file)
	}
}

// reloadAllowedPubkeys reloads the allowed pubkeys list from the allowed_pubkeys.csv file.
// It returns the number of pubkeys in the new list.
func (c *Controller) reloadAllowedPubkeys() (int, error) {
	path := filepath.Join(c.dir, PubkeysAllowedFile)
	pubkeys, _, err := parseCSV(path)
	if err != nil {
		return 0, err
	}

	for i := range pubkeys {
		pk, err := toPubkey(pubkeys[i])
		if err != nil {
			return 0, fmt.Errorf("invalid pubkey at line %d: %w", i+1, err)
		}
		pubkeys[i] = pk
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.pubkeysAllowed.Clear()
	c.pubkeysAllowed = smallset.NewFrom(pubkeys...)
	return c.pubkeysAllowed.Size(), nil
}

// reloadBlockedPubkeys reloads the blocked pubkeys list from the blocked_pubkeys.csv file.
// It returns the number of pubkeys in the new list.
func (c *Controller) reloadBlockedPubkeys() (int, error) {
	path := filepath.Join(c.dir, PubkeysBlockedFile)
	pubkeys, _, err := parseCSV(path)
	if err != nil {
		return 0, err
	}

	for i := range pubkeys {
		pk, err := toPubkey(pubkeys[i])
		if err != nil {
			return 0, fmt.Errorf("invalid pubkey at line %d: %w", i+1, err)
		}
		pubkeys[i] = pk
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.pubkeysBlocked.Clear()
	c.pubkeysBlocked = smallset.NewFrom(pubkeys...)
	return c.pubkeysBlocked.Size(), nil
}

// reloadBlockedPlatformUsers reloads the blocked platform users list.
// Format: "platform:username,reason"
func (c *Controller) reloadBlockedPlatformUsers() (int, error) {
	path := filepath.Join(c.dir, PlatformUsersBlockedFile)
	entries, _, err := parseCSV(path)
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.platformUsersBlocked.Clear()
	c.platformUsersBlocked = smallset.NewFrom(entries...)
	return c.platformUsersBlocked.Size(), nil
}

// toPubkey tries to parse a string into a hex pubkey.
// It supports both hex and npub formats.
func toPubkey(pk string) (string, error) {
	if nostr.IsValid32ByteHex(pk) {
		return pk, nil
	}

	if strings.HasPrefix(pk, "npub1") {
		_, data, err := nip19.Decode(pk)
		if err != nil {
			return "", fmt.Errorf("invalid pubkey: %w", err)
		}
		return data.(string), nil
	}

	return "", fmt.Errorf("invalid pubkey: %s", pk)
}

// parseCSV parses a CSV file with exactly two columns.
// The only lines skipped are the ones starting with # (comments).
func parseCSV(path string) (col1 []string, col2 []string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open CSV file: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comment = '#'
	reader.FieldsPerRecord = 2
	reader.TrimLeadingSpace = true

	line := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}

		line++
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read CSV at line %d: %w", line, err)
		}

		col1 = append(col1, strings.TrimSpace(record[0]))
		col2 = append(col2, strings.TrimSpace(record[1]))
	}
	return col1, col2, nil
}
