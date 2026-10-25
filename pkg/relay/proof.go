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
// signature. Older pending proofs from that pubkey for that certificate are
// dropped. When it replaces the stored proof, the listings that proof let
// the owner and its delegate publish for that certificate are removed:
// selection does not read proofs, so a revoked key would otherwise keep its
// slot.
func (r *T) storeRevocation(ctx context.Context, event *nostr.Event) error {
	cert, _ := events.Find(event.Tags, "d")
	publishers := []string{event.PubKey}
	previous, err := r.store.Query(ctx, nostr.Filter{
		Kinds:   []int{events.KindIdentityProof},
		Authors: []string{event.PubKey},
		Tags:    nostr.TagMap{"d": []string{cert}},
		Limit:   1,
	})
	if err != nil {
		return err
	}
	if len(previous) > 0 {
		publishers = proofPublishers(previous[0])
	}
	if _, err := r.store.Replace(ctx, event); err != nil {
		return err
	}
	if _, err := r.dropPending(ctx, func(other nostr.Event) bool {
		d, _ := events.Find(other.Tags, "d")
		return other.Kind == events.KindIdentityProof && other.PubKey == event.PubKey && d == cert && !newerEvent(other, *event)
	}); err != nil {
		return err
	}
	current, err := r.store.Has(ctx, nostr.Filter{IDs: []string{event.ID}})
	if err != nil || !current {
		return err
	}
	return r.dropRevokedListings(ctx, cert, publishers)
}

// dropRevokedListings deletes the listings publishers signed for every app
// whose certificate is cert. The catalog key's listing stays.
func (r *T) dropRevokedListings(ctx context.Context, cert string, publishers []string) error {
	assets, err := r.listedAssets(ctx, cert, 0)
	if err != nil {
		return err
	}
	var apps []string
	for _, asset := range assets {
		if appID, ok := events.Find(asset.Tags, "i"); ok && appID != "" && !slices.Contains(apps, appID) {
			apps = append(apps, appID)
		}
	}
	var ids []string
	for _, appID := range apps {
		for _, pub := range publishers {
			if pub == r.config.Info.Pubkey {
				continue
			}
			found, err := eventsByApp(ctx, r.store, pub, appID)
			if err != nil {
				return err
			}
			ids = append(ids, found...)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.store.Delete(ctx, nostr.Filter{IDs: ids}); err != nil {
		return err
	}
	slog.Info("relay: revocation removed developer listings", "certificate", cert, "apps", apps, "events", len(ids))
	return nil
}

// proofHasAPK reports whether a proof has an APK to be checked against: an
// app listed with its certificate, or a pending asset naming that certificate
// from the proof's owner or delegate.
func (r *T) proofHasAPK(ctx context.Context, proof *nostr.Event) (bool, error) {
	cert, _ := events.Find(proof.Tags, "d")
	assets, err := r.listedAssets(ctx, cert, 1)
	if err != nil || len(assets) > 0 {
		return len(assets) > 0, err
	}
	pending, err := r.store.QueryPending(ctx, events.KindAsset)
	if err != nil {
		return false, err
	}
	publishers := proofPublishers(*proof)
	for _, asset := range pending {
		if slices.Contains(publishers, asset.PubKey) && assetSigner(asset) == strings.ToLower(cert) {
			return true, nil
		}
	}
	return false, nil
}

// promoteClaim checks a pending proof against the newest APK listed with its
// certificate. It reports whether the proof was promoted. An APK not read
// yet is queued for the reader.
func (r *T) promoteClaim(ctx context.Context, proof nostr.Event, skipBroadcast string) (bool, error) {
	cert, _ := events.Find(proof.Tags, "d")
	assets, err := r.listedAssets(ctx, cert, 1)
	if err != nil || len(assets) == 0 {
		return false, err
	}
	x, _ := events.Find(assets[0].Tags, "x")
	chain, readErr, ok := r.apkSigning(x, r.assetURL(assets[0].Tags))
	if !ok || readErr != "" {
		return false, nil
	}
	if c := chain.cert(strings.ToLower(cert)); c != nil {
		return r.commitCheckedProof(ctx, proof, c, skipBroadcast)
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

// checkedProofs returns the pending proofs whose d tag is in chain and whose
// signature verifies with that certificate. A proof that does not verify is dropped.
func (r *T) checkedProofs(ctx context.Context, chain signing) ([]nostr.Event, error) {
	pending, err := r.store.QueryPending(ctx, events.KindIdentityProof)
	if err != nil {
		return nil, err
	}
	var out []nostr.Event
	for _, proof := range pending {
		d, _ := events.Find(proof.Tags, "d")
		cert := chain.cert(strings.ToLower(d))
		if cert == nil {
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
// certificate is cert, newest first. limit, when positive, caps them.
func (r *T) listedAssets(ctx context.Context, cert string, limit int) ([]nostr.Event, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		WHERE e.kind = ?
		  AND e.id IN (
		    SELECT event_id FROM tags
		    WHERE key = 'apk_certificate_hash' AND lower(value) = ?
		  )
		ORDER BY e.created_at DESC
		LIMIT ?
	`, events.KindAsset, strings.ToLower(cert), limit)
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
		if _, ok := events.Find(event.Tags, "x"); ok && assetSigner(event) == cert {
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

// assetSigner is the first apk_certificate_hash. Further hashes are ignored.
func assetSigner(asset nostr.Event) string {
	hash, ok := events.Find(asset.Tags, "apk_certificate_hash")
	if !ok {
		return ""
	}
	return strings.ToLower(hash)
}

// assetAncestors are the apk_certificate_ancestor values, oldest first.
func assetAncestors(asset nostr.Event) []string {
	hashes := events.FindAll(asset.Tags, "apk_certificate_ancestor")
	for i := range hashes {
		hashes[i] = strings.ToLower(hashes[i])
	}
	return hashes
}

// assetNames reports whether cert is the asset's signer or one of its ancestors.
func assetNames(asset nostr.Event, cert string) bool {
	cert = strings.ToLower(cert)
	if assetSigner(asset) == cert {
		return true
	}
	return slices.Contains(assetAncestors(asset), cert)
}

// claimMatches reports whether the asset's signer and ancestors are the chain read from the APK.
func claimMatches(asset nostr.Event, chain signing) bool {
	if assetSigner(asset) != chain.signer() {
		return false
	}
	return slices.Equal(assetAncestors(asset), chain.ancestors())
}
