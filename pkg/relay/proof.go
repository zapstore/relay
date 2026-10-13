package relay

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

var errProofWithoutAPK = errors.New("kind 30509: no APK to check this certificate against. A claim alone is accepted only for a listed app; for a new app, send the proof with its first release")

const msgProofPending = "the proof will be saved when it is checked against an APK"

// holdProof keeps a new active kind 30509 in the buffer zone until it is
// checked against an APK: the promoted APK of an app listed with its
// certificate, else the buffered APK of the listing it came with.
func (r *T) holdProof(ctx context.Context, event *nostr.Event) persistResult {
	found, err := r.proofHasAPK(ctx, event)
	if err != nil {
		slog.Error("relay: failed to look up the proof's apk", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if !found {
		return persistResult{msg: errProofWithoutAPK.Error()}
	}
	if _, err := r.store.SavePending(ctx, event); err != nil {
		slog.Error("relay: failed to save pending proof", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if err := supersedePending(ctx, r.store, event); err != nil {
		slog.Error("relay: failed to supersede pending proof", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}

	if _, err := r.promoteClaim(ctx, *event, event.ID); err != nil {
		slog.Error("relay: failed to promote proof", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if err := r.promoteListingsBy(ctx, proofPublishers(*event), event.ID); err != nil {
		slog.Error("relay: failed to promote listing with proof", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}

	stored, err := r.store.Has(ctx, nostr.Filter{IDs: []string{event.ID}})
	if err != nil {
		slog.Error("relay: failed to check promoted proof", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if stored {
		return persistResult{ok: true, broadcast: true}
	}
	return persistResult{ok: true, msg: msgProofPending}
}

// storeRevocation stores a revocation on arrival. It needs only the Nostr
// signature. Older pending proofs from that pubkey for that certificate are dropped.
func (r *T) storeRevocation(ctx context.Context, event *nostr.Event) error {
	if _, err := r.store.Replace(ctx, event); err != nil {
		return err
	}
	cert, _ := events.Find(event.Tags, "d")
	_, err := r.dropPending(ctx, func(other nostr.Event) bool {
		d, _ := events.Find(other.Tags, "d")
		return other.Kind == events.KindIdentityProof && other.PubKey == event.PubKey && d == cert && !newerEvent(other, *event)
	})
	return err
}

// proofHasAPK reports whether a proof has an APK to be checked against: an
// app listed with its certificate, or a pending asset naming that certificate
// from the proof's owner or delegate.
func (r *T) proofHasAPK(ctx context.Context, proof *nostr.Event) (bool, error) {
	cert, _ := events.Find(proof.Tags, "d")
	assets, err := r.listedAssets(ctx, cert)
	if err != nil || len(assets) > 0 {
		return len(assets) > 0, err
	}
	pending, err := r.store.QueryPending(ctx, events.KindAsset)
	if err != nil {
		return false, err
	}
	publishers := proofPublishers(*proof)
	for _, asset := range pending {
		if slices.Contains(publishers, asset.PubKey) && slices.Contains(assetCerts(asset), cert) {
			return true, nil
		}
	}
	return false, nil
}

// promoteClaim checks a pending proof against the promoted APK of an app
// listed with its certificate. It reports whether the proof was promoted.
// An APK that has not been read yet is queued for the reader.
func (r *T) promoteClaim(ctx context.Context, proof nostr.Event, skipBroadcast string) (bool, error) {
	cert, _ := events.Find(proof.Tags, "d")
	assets, err := r.listedAssets(ctx, cert)
	if err != nil || len(assets) == 0 {
		return false, err
	}
	var unread *nostr.Event
	for i := range assets {
		x, _ := events.Find(assets[i].Tags, "x")
		ders, readErr, ok, err := r.store.Signing(ctx, x)
		if err != nil {
			return false, err
		}
		if !ok {
			if unread == nil {
				unread = &assets[i]
			}
			continue
		}
		if readErr != "" {
			continue
		}
		chain, err := parseSigning(ders)
		if err != nil {
			return false, err
		}
		if c := chain.cert(cert); c != nil {
			return r.commitCheckedProof(ctx, proof, c, skipBroadcast)
		}
	}
	if unread != nil {
		x, _ := events.Find(unread.Tags, "x")
		r.apks.request(apkJob{blob: x, url: assetURL(unread.Tags)})
	}
	return false, nil
}

// commitCheckedProof promotes a proof whose signature verifies with cert, a
// certificate read from an APK. A proof that does not verify is dropped.
func (r *T) commitCheckedProof(ctx context.Context, proof nostr.Event, cert *x509.Certificate, skipBroadcast string) (bool, error) {
	if err := events.VerifyIdentityProof(&proof, cert); err != nil {
		slog.Warn("proof: refused", "event", proof.ID, "error", err)
		return false, r.store.DeletePending(ctx, proof.ID)
	}
	if err := r.commitProof(ctx, proof, skipBroadcast); err != nil {
		return false, err
	}
	return true, nil
}

func (r *T) commitProof(ctx context.Context, proof nostr.Event, skipBroadcast string) error {
	stored, err := r.store.Replace(ctx, &proof)
	if err != nil {
		return err
	}
	if err := r.store.DeletePending(ctx, proof.ID); err != nil {
		return err
	}
	if !stored {
		return nil
	}
	slog.Info("proof promoted", "event", proof.ID, "pubkey", proof.PubKey)
	if r.server != nil && proof.ID != skipBroadcast {
		if err := r.server.Broadcast(&proof); err != nil {
			slog.Error("relay: failed to broadcast proof", "event", proof.ID, "error", err)
		}
	}
	return nil
}

// checkedProofs returns the pending active proofs for hash whose signature
// verifies with cert. A proof that does not verify is dropped.
func (r *T) checkedProofs(ctx context.Context, hash string, cert *x509.Certificate) ([]nostr.Event, error) {
	pending, err := r.store.QueryPending(ctx, events.KindIdentityProof)
	if err != nil {
		return nil, err
	}
	var out []nostr.Event
	for _, proof := range pending {
		if d, _ := events.Find(proof.Tags, "d"); d != hash {
			continue
		}
		if err := events.VerifyIdentityProof(&proof, cert); err != nil {
			slog.Warn("proof: refused", "event", proof.ID, "error", err)
			if err := r.store.DeletePending(ctx, proof.ID); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, proof)
	}
	return out, nil
}

// listedAssets returns the stored assets of every listed app whose app
// certificate is cert, newest first.
func (r *T) listedAssets(ctx context.Context, cert string) ([]nostr.Event, error) {
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND t.key = 'i'
		  AND t.value IN (SELECT app_id FROM app_certificate WHERE hash = ?)
		ORDER BY e.created_at DESC
	`, events.KindAsset, cert)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nostr.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		if _, ok := events.Find(event.Tags, "x"); ok {
			out = append(out, event)
		}
	}
	return out, rows.Err()
}

// proofPublishers are the pubkeys a proof lets publish: its owner and delegate.
func proofPublishers(proof nostr.Event) []string {
	out := []string{proof.PubKey}
	if delegate, ok := events.Find(proof.Tags, "delegation"); ok && delegate != "" {
		out = append(out, delegate)
	}
	return out
}

// assetCerts returns an asset's apk_certificate_hash values in lower case.
func assetCerts(asset nostr.Event) []string {
	hashes := events.FindAll(asset.Tags, "apk_certificate_hash")
	for i := range hashes {
		hashes[i] = strings.ToLower(hashes[i])
	}
	return hashes
}
