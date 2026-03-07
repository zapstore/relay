// Package linkverify implements NIP-C1 certificate link verification as a relay save hook.
// When a kind 30509 (IdentityProof) or kind 3063 (SoftwareAsset) event is saved,
// this package cross-checks them: if a matching pair exists, it downloads the APK
// from https://<blossomHostname>/<sha256-hash>, extracts the signing certificate,
// verifies the NIP-C1 signature, and auto-whitelists the pubkey on success.
package linkverify

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/avast/apkverifier"
	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/acl"
	"github.com/zapstore/relay/pkg/events"
)

// Verifier performs NIP-C1 certificate verification after events are saved.
type Verifier struct {
	db           *sqlite.Store
	acl          *acl.Controller
	blossomBase  string // e.g., "https://cdn.zapstore.dev"
	log          *slog.Logger
}

// New creates a new Verifier. blossomHostname is the Blossom CDN hostname (e.g. "cdn.zapstore.dev").
func New(db *sqlite.Store, acl *acl.Controller, blossomHostname string, log *slog.Logger) *Verifier {
	return &Verifier{
		db:          db,
		acl:         acl,
		blossomBase: "https://" + blossomHostname,
		log:         log,
	}
}

// OnEvent is called after an event is saved. It triggers C1 verification for
// kind 30509 and kind 3063 events.
func (v *Verifier) OnEvent(ctx context.Context, event *nostr.Event) {
	switch event.Kind {
	case events.KindIdentityProof:
		go v.verifyFromProof(context.Background(), event)
	case events.KindAsset:
		go v.verifyFromAsset(context.Background(), event)
	}
}

// verifyFromProof is triggered when a 30509 event arrives.
// It looks for existing 3063 events from the same pubkey with a matching cert hash.
func (v *Verifier) verifyFromProof(ctx context.Context, proofEvent *nostr.Event) {
	certHash, ok := events.Find(proofEvent.Tags, "d")
	if !ok || certHash == "" {
		return
	}

	filter := nostr.Filter{
		Kinds:   []int{events.KindAsset},
		Authors: []string{proofEvent.PubKey},
		Tags:    nostr.TagMap{"apk_certificate_hash": []string{certHash}},
		Limit:   1,
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	assetEvents, err := v.db.Query(ctx, filter)
	if err != nil || len(assetEvents) == 0 {
		return
	}

	v.runVerification(ctx, proofEvent, &assetEvents[0])
}

// verifyFromAsset is triggered when a 3063 event arrives.
// It looks for an existing 30509 from the same pubkey matching the cert hash.
func (v *Verifier) verifyFromAsset(ctx context.Context, assetEvent *nostr.Event) {
	certHashes := findAll(assetEvent.Tags, "apk_certificate_hash")
	if len(certHashes) == 0 {
		return
	}

	filter := nostr.Filter{
		Kinds:   []int{events.KindIdentityProof},
		Authors: []string{assetEvent.PubKey},
		Tags:    nostr.TagMap{"d": []string{certHashes[0]}},
		Limit:   1,
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	proofEvents, err := v.db.Query(ctx, filter)
	if err != nil || len(proofEvents) == 0 {
		return
	}

	v.runVerification(ctx, &proofEvents[0], assetEvent)
}

// runVerification performs the full C1 verification pipeline.
func (v *Verifier) runVerification(ctx context.Context, proofEvent, assetEvent *nostr.Event) {
	certHash, _ := events.Find(proofEvent.Tags, "d")
	pubkey := proofEvent.PubKey

	// Already whitelisted — nothing to do
	allowed, _ := v.acl.AllowPubkey(ctx, pubkey)
	if allowed {
		return
	}

	// Only auto-whitelist if the app has already been indexed (kind 32267 exists)
	appID, ok := events.Find(assetEvent.Tags, "i")
	if !ok || appID == "" {
		v.log.Warn("linkverify: asset event missing 'i' tag, skipping C1 whitelist", "event", assetEvent.ID)
		return
	}
	appFilter := nostr.Filter{
		Kinds: []int{events.KindApp},
		Tags:  nostr.TagMap{"d": []string{appID}},
		Limit: 1,
	}
	appEvents, err := v.db.Query(ctx, appFilter)
	if err != nil || len(appEvents) == 0 {
		v.log.Info("linkverify: app not yet indexed, deferring C1 whitelist", "app_id", appID, "pubkey", pubkey[:16])
		return
	}

	// Get APK SHA-256 hash from asset event
	apkHash, ok := events.Find(assetEvent.Tags, "x")
	if !ok || apkHash == "" {
		v.log.Warn("linkverify: asset event missing 'x' tag", "event", assetEvent.ID)
		return
	}

	// Download APK directly from the Blossom CDN by hash
	apkURL := fmt.Sprintf("%s/%s", v.blossomBase, apkHash)
	apkPath, err := v.downloadAPK(ctx, apkURL)
	if err != nil {
		v.log.Warn("linkverify: failed to download APK", "url", apkURL, "error", err)
		return
	}
	defer os.Remove(apkPath)

	// Extract cert from APK
	cert, err := extractCert(apkPath)
	if err != nil {
		v.log.Warn("linkverify: failed to extract cert", "error", err)
		return
	}

	// Confirm cert hash matches 30509 d tag
	extractedHash := computeCertHash(cert)
	if extractedHash != certHash {
		v.log.Warn("linkverify: cert hash mismatch",
			"expected", certHash, "got", extractedHash, "pubkey", pubkey)
		return
	}

	// Verify 30509 signature against cert's public key
	if err := verifyProofSignature(proofEvent, pubkey, cert); err != nil {
		v.log.Warn("linkverify: signature verification failed", "pubkey", pubkey, "error", err)
		return
	}

	// Auto-whitelist
	if err := v.acl.AppendAllowedPubkey(pubkey, fmt.Sprintf("C1-verified cert %s", certHash[:16])); err != nil {
		v.log.Error("linkverify: failed to whitelist pubkey", "pubkey", pubkey, "error", err)
		return
	}

	v.log.Info("linkverify: auto-whitelisted via C1 verification",
		"pubkey", pubkey, "cert_prefix", certHash[:16])
}

// downloadAPK downloads an APK from the given URL and returns the temp file path.
func (v *Verifier) downloadAPK(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "linkverify-*.apk")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("failed to write APK: %w", err)
	}

	return f.Name(), nil
}

// extractCert extracts the signing certificate from an APK.
func extractCert(apkPath string) (*x509.Certificate, error) {
	res, err := apkverifier.Verify(apkPath, nil)
	if err != nil {
		return nil, fmt.Errorf("APK verification failed: %w", err)
	}

	_, cert := apkverifier.PickBestApkCert(res.SignerCerts)
	if cert == nil {
		return nil, fmt.Errorf("no valid certificate found in APK")
	}

	return cert, nil
}

// computeCertHash returns the SHA-256 hash of the DER-encoded certificate as hex.
func computeCertHash(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(h[:])
}

// verifyProofSignature verifies the NIP-C1 signature in the 30509 event
// against the certificate's public key.
func verifyProofSignature(proofEvent *nostr.Event, pubkeyHex string, cert *x509.Certificate) error {
	sigStr, ok := events.Find(proofEvent.Tags, "signature")
	if !ok {
		return fmt.Errorf("missing 'signature' tag")
	}

	expiryStr, ok := events.Find(proofEvent.Tags, "expiry")
	if !ok {
		return fmt.Errorf("missing 'expiry' tag")
	}

	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid expiry: %w", err)
	}

	createdAt := proofEvent.CreatedAt.Time().Unix()
	message := fmt.Sprintf(
		"Verifying at %d until %d that I control the following Nostr public key: %s",
		createdAt, expiry, pubkeyHex,
	)

	sig, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	switch pubKey := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		h := sha256.Sum256([]byte(message))
		if !ecdsa.VerifyASN1(pubKey, h[:], sig) {
			return fmt.Errorf("ECDSA signature verification failed")
		}
	case *rsa.PublicKey:
		h := sha256.Sum256([]byte(message))
		if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, h[:], sig); err != nil {
			pssOpts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}
			if err2 := rsa.VerifyPSS(pubKey, crypto.SHA256, h[:], sig, pssOpts); err2 != nil {
				return fmt.Errorf("RSA signature verification failed: %w", err)
			}
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(pubKey, []byte(message), sig) {
			return fmt.Errorf("Ed25519 signature verification failed")
		}
	default:
		return fmt.Errorf("unsupported public key type: %T", cert.PublicKey)
	}

	return nil
}

// findAll returns all tag values for the given key.
func findAll(tags nostr.Tags, key string) []string {
	var values []string
	for _, tag := range tags {
		if len(tag) > 1 && tag[0] == key {
			values = append(values, tag[1])
		}
	}
	return values
}
