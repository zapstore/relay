package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func (r *T) enqueueProfile(pubkey string) {
	select {
	case r.profileJobs <- pubkey:
	default:
		slog.Warn("profile processor queue is full", "pubkey", pubkey)
	}
}

func (r *T) runProfileWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pubkey := <-r.profileJobs:
			if err := r.processProfile(ctx, pubkey); err != nil {
				slog.Warn("profile processing failed", "pubkey", pubkey, "error", err)
			}
		}
	}
}

func (r *T) processProfile(ctx context.Context, pubkey string) error {
	if !nostr.IsValid32ByteHex(pubkey) {
		return fmt.Errorf("invalid profile pubkey %q", pubkey)
	}
	return r.fetchProfile(ctx, pubkey)
}

func (r *T) fetchProfile(ctx context.Context, pubkey string) error {
	var latest *nostr.Event
	var errs []error

	for _, relayURL := range r.config.ProfileRelays {
		queryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		upstream, err := nostr.RelayConnect(queryCtx, relayURL)
		if err != nil {
			cancel()
			errs = append(errs, fmt.Errorf("%s: %w", relayURL, err))
			continue
		}

		found, err := upstream.QuerySync(queryCtx, nostr.Filter{
			Kinds:   []int{events.KindProfile},
			Authors: []string{pubkey},
			Limit:   1,
		})
		_ = upstream.Close()
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", relayURL, err))
			continue
		}
		for _, event := range found {
			if event.Kind != events.KindProfile || event.PubKey != pubkey {
				continue
			}
			if ok, err := event.CheckSignature(); err != nil || !ok {
				continue
			}
			if latest == nil || event.CreatedAt > latest.CreatedAt {
				latest = event
			}
		}
	}

	if latest == nil {
		if len(errs) == 0 {
			return errors.New("profile not found on configured relays")
		}
		return fmt.Errorf("profile not found on configured relays: %w", errors.Join(errs...))
	}

	saved, err := r.store.Replace(ctx, latest)
	if err != nil {
		return fmt.Errorf("failed to save fetched kind 0: %w", err)
	}
	if !saved {
		return nil
	}
	if err := r.server.Broadcast(latest); err != nil {
		slog.Debug("failed to broadcast fetched kind 0", "event", latest.ID, "error", err)
	}
	slog.Info("kind 0 updated", "pubkey", pubkey, "event", latest.ID)
	return nil
}
