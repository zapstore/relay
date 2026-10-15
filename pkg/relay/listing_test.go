package relay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
)

const testApp = "com.example.app"

var (
	catalogKey = strings.Repeat("c", 64)
	ownerKey   = strings.Repeat("a", 64)
)

func TestListingStaysPendingUntilComplete(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	x := blobs.put("apk")
	app, release, asset := testTriple("a", catalogKey, x, strings.Repeat("e", 64), 1)

	if res := r.holdListing(t.Context(), app); res.msg != msgListingPending || !res.ok || res.broadcast {
		t.Fatalf("app: %+v", res)
	}
	if res := r.holdListing(t.Context(), release); res.msg != msgListingPending || res.broadcast {
		t.Fatalf("release: %+v", res)
	}
	if slotHas(t, r.store, app.ID) || slotHas(t, r.store, release.ID) {
		t.Fatal("stored before the triple was complete")
	}
	if len(blobs.promoted) != 0 {
		t.Fatal("a file was promoted before its listing")
	}

	res := r.holdListing(t.Context(), asset)
	if !res.ok || !res.broadcast || res.msg != "" {
		t.Fatalf("asset: %+v", res)
	}
	for _, id := range []string{app.ID, release.ID, asset.ID} {
		if !slotHas(t, r.store, id) {
			t.Fatalf("missing %s", id)
		}
		if kept, err := pendingHas(t.Context(), r.store, id); err != nil || kept {
			t.Fatalf("pending %s: kept %v err %v", id, kept, err)
		}
	}
	if len(blobs.promoted) != 1 || blobs.promoted[0] != x {
		t.Fatalf("promoted %v, want the asset file", blobs.promoted)
	}
	if len(blobs.opened) != 0 {
		t.Fatal("the indexer's apk was read")
	}
}

func TestBannedHashDropsPendingListing(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	pol, err := policy.Open(r.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	r.pol = pol
	hash := strings.Repeat("ab", 32)
	if err := pol.RecordBan(t.Context(), policy.Publisher{Kind: policy.KindNostr, ID: strings.Repeat("11", 32)}, "bad file", []string{hash}, nil); err != nil {
		t.Fatal(err)
	}
	app, release, asset := testTriple("b", catalogKey, hash, strings.Repeat("e", 64), 1)
	if res := r.holdListing(t.Context(), app); !res.ok {
		t.Fatalf("app: %+v", res)
	}
	if res := r.holdListing(t.Context(), release); !res.ok {
		t.Fatalf("release: %+v", res)
	}
	res := r.holdListing(t.Context(), asset)
	if res.ok || res.broadcast || res.msg != ErrFileNotAllowed.Error() {
		t.Fatalf("asset: %+v", res)
	}
	for _, id := range []string{release.ID, asset.ID} {
		kept, err := pendingHas(t.Context(), r.store, id)
		if err != nil || kept {
			t.Fatalf("pending %s: kept %v err %v", id, kept, err)
		}
		if slotHas(t, r.store, id) {
			t.Fatalf("stored %s", id)
		}
	}
	banned, err := pol.Blacklisted(t.Context(), policy.Publisher{Kind: policy.KindNostr, ID: catalogKey})
	if err != nil {
		t.Fatal(err)
	}
	if banned {
		t.Fatal("catalog key was blacklisted")
	}
}

func TestNotAllowedRejectsBannedHash(t *testing.T) {
	db := newSlotDB(t)
	pol, err := policy.Open(db.DB)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("cd", 32)
	if err := pol.RecordBan(t.Context(), policy.Publisher{Kind: policy.KindNostr, ID: strings.Repeat("11", 32)}, "bad file", []string{hash}, nil); err != nil {
		t.Fatal(err)
	}
	err = NotAllowed(pol)(nil, &nostr.Event{
		Kind: events.KindAsset, PubKey: catalogKey, Tags: nostr.Tags{{"x", hash}},
	})
	if !errors.Is(err, ErrFileNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	banned, err := pol.Blacklisted(t.Context(), policy.Publisher{Kind: policy.KindNostr, ID: catalogKey})
	if err != nil {
		t.Fatal(err)
	}
	if banned {
		t.Fatal("catalog key was blacklisted")
	}
}

func TestDeletionMatchingNothingIsRejected(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	event := &nostr.Event{
		ID: strings.Repeat("d", 64), PubKey: ownerKey, Kind: nostr.KindDeletion,
		Tags: nostr.Tags{{"e", strings.Repeat("e", 64)}},
	}
	res := r.persist(nil, event)
	if res.ok || res.broadcast || res.msg != errNothingDeleted.Error() {
		t.Fatalf("%+v", res)
	}
	if slotHas(t, r.store, event.ID) {
		t.Fatal("deletion was stored")
	}
}

func TestListingWaitsForBlossom(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	app, release, asset := testTriple("a", catalogKey, strings.Repeat("f", 64), strings.Repeat("e", 64), 1)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), release)
	if res := r.holdListing(t.Context(), asset); res.msg != msgBlobPending {
		t.Fatalf("msg %q", res.msg)
	}
	if slotHas(t, r.store, app.ID) || slotHas(t, r.store, asset.ID) {
		t.Fatal("listing stored without a blossom blob")
	}
}

func TestIndexerPinsCertificateAtPromotion(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	old, next := strings.Repeat("1", 64), strings.Repeat("2", 64)
	prior := testAsset("p", catalogKey, strings.Repeat("0", 64), old, 1)
	prior.CreatedAt = 1
	saveSlotEvent(t, r.store, *prior)
	app, release, asset := testTriple("a", catalogKey, blobs.put("apk"), next, 1)

	r.holdListing(t.Context(), asset)
	if pin := pinned(t, r); pin != old {
		t.Fatalf("a pending asset moved the pin to %s", pin)
	}
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), release)
	if pin := pinned(t, r); pin != next {
		t.Fatalf("pin %s, want the indexer's certificate", pin)
	}
}

func TestNewDeveloperAppPromotesWithItsProof(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"dev-apk": {key.cert}})
	app, release, asset := testTriple("b", ownerKey, blobs.put("dev-apk"), key.hash, 1)

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)
	drainReads(t, r)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("a developer listing was promoted without a proof")
	}

	proof := key.proof(t, "4", ownerKey, "")
	res := r.persist(nil, proof)
	if !res.ok || !res.broadcast {
		t.Fatalf("proof: %+v", res)
	}
	for _, id := range []string{app.ID, release.ID, asset.ID, proof.ID} {
		if !slotHas(t, r.store, id) {
			t.Fatalf("missing %s", id)
		}
	}
	if pin := pinned(t, r); pin != key.hash {
		t.Fatalf("pin %s, want the apk signer", pin)
	}
}

func TestDeveloperAPKMustBeSignedByTheAppCertificate(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	app, other := newTestKey(t), newTestKey(t)
	prior := testAsset("p", ownerKey, strings.Repeat("0", 64), app.hash, 1)
	prior.CreatedAt = 1
	saveSlotEvent(t, r.store, *prior)
	saveSlotEvent(t, r.store, *app.proof(t, "4", ownerKey, ""))
	r.readAPK = readChains(t, map[string]signing{"dev-apk": {other.cert}})
	appEvent, release, asset := testTriple("b", ownerKey, blobs.put("dev-apk"), app.hash, 1)

	r.holdListing(t.Context(), appEvent)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)
	drainReads(t, r)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("promoted an apk signed by another certificate")
	}
	for _, id := range []string{release.ID, asset.ID} {
		if kept, _ := pendingHas(t.Context(), r.store, id); kept {
			t.Fatalf("refused %s is still pending", id)
		}
	}
}

func TestDeveloperRotationReachesTheSlot(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	old, next := newTestKey(t), newTestKey(t)
	prior := testAsset("p", ownerKey, strings.Repeat("0", 64), old.hash, 1)
	prior.CreatedAt = 1
	saveSlotEvent(t, r.store, *prior)
	saveSlotEvent(t, r.store, *old.proof(t, "4", ownerKey, ""))
	r.readAPK = readChains(t, map[string]signing{"rotated": {old.cert, next.cert}})
	app, release, asset := testTriple("b", ownerKey, blobs.put("rotated"), next.hash, 1)
	asset.Tags = append(asset.Tags, nostr.Tag{"apk_certificate_hash", old.hash})

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)
	drainReads(t, r)
	if !slotHas(t, r.store, release.ID) {
		t.Fatal("an apk signed by a successor in the lineage was not promoted")
	}
	if pin := pinned(t, r); pin != next.hash {
		t.Fatalf("certificate %s, want the current asset's signer", pin)
	}
	var named int
	err := r.store.DB.QueryRow(`
		SELECT COUNT(*) FROM tags
		WHERE event_id = ? AND key = 'apk_certificate_hash' AND value = ?
	`, asset.ID, old.hash).Scan(&named)
	if err != nil || named != 1 {
		t.Fatalf("stored asset names the original certificate: %d %v", named, err)
	}
}

func TestProofWithoutAPKIsRefused(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	proof := newTestKey(t).proof(t, "4", ownerKey, "")
	res := r.persist(nil, proof)
	if res.ok || res.msg != errProofWithoutAPK.Error() {
		t.Fatalf("res %+v", res)
	}
	if kept, _ := pendingHas(t.Context(), r.store, proof.ID); kept {
		t.Fatal("a refused proof is pending")
	}
}

func TestClaimIsCheckedAgainstThePromotedAPK(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key, stranger := newTestKey(t), newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"apk": {key.cert}})
	promoteIndexer(t, r, blobs.put("apk"), key.hash)

	forged := stranger.proof(t, "5", strings.Repeat("d", 64), "")
	forged.Tags[0] = nostr.Tag{"d", key.hash}
	if res := r.persist(nil, forged); !res.ok || res.msg != msgProofPending {
		t.Fatalf("forged: %+v", res)
	}
	proof := key.proof(t, "4", ownerKey, "")
	if res := r.persist(nil, proof); !res.ok || res.msg != msgProofPending {
		t.Fatalf("proof: %+v", res)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, proof.ID) {
		t.Fatal("the claim was not promoted")
	}
	if slotHas(t, r.store, forged.ID) {
		t.Fatal("a proof the certificate did not sign was promoted")
	}
	if kept, _ := pendingHas(t.Context(), r.store, forged.ID); kept {
		t.Fatal("the forged proof is still pending")
	}
}

func TestClaimFallsBackToTheAssetURL(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"apk": {key.cert}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("apk"))
	}))
	t.Cleanup(srv.Close)
	x := blobs.put("apk")
	promoteIndexer(t, r, x, key.hash, nostr.Tag{"url", srv.URL + "/app.apk"})
	delete(blobs.bodies, x)

	proof := key.proof(t, "4", ownerKey, "")
	r.persist(nil, proof)
	drainReads(t, r)
	if !slotHas(t, r.store, proof.ID) {
		t.Fatal("the claim was not checked against the asset url")
	}
}

func TestRevocationIsStoredOnArrival(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	key := newTestKey(t)
	proof := key.proof(t, "4", ownerKey, "")
	if _, err := r.store.SavePending(t.Context(), proof); err != nil {
		t.Fatal(err)
	}
	revocation := &nostr.Event{
		ID: strings.Repeat("5", 64), PubKey: ownerKey, Kind: events.KindIdentityProof,
		CreatedAt: proof.CreatedAt + 1, Tags: nostr.Tags{{"d", key.hash}, {"revoked"}},
	}
	if res := r.persist(nil, revocation); !res.ok || !res.broadcast {
		t.Fatalf("res %+v", res)
	}
	if !slotHas(t, r.store, revocation.ID) {
		t.Fatal("revocation was not stored")
	}
	if kept, _ := pendingHas(t.Context(), r.store, proof.ID); kept {
		t.Fatal("the older proof is still pending")
	}
}

func TestVersionGuard(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	promoteIndexer(t, r, blobs.put("v5"), strings.Repeat("e", 64))

	lower := testAsset("l", catalogKey, blobs.put("v4"), strings.Repeat("e", 64), 4)
	lowerRelease := testRelease("m", catalogKey, "0.9.0", "main", lower.ID)
	r.holdListing(t.Context(), lower)
	res := r.holdListing(t.Context(), lowerRelease)
	if res.ok || !strings.Contains(res.msg, "version_code 4 is lower than 5") {
		t.Fatalf("lower: %+v", res)
	}

	equal := testAsset("n", catalogKey, blobs.put("v5b"), strings.Repeat("e", 64), 5)
	r.holdListing(t.Context(), equal)
	if res := r.holdListing(t.Context(), testRelease("o", catalogKey, "1.0.1", "main", equal.ID)); !res.broadcast {
		t.Fatalf("equal: %+v", res)
	}

	beta := testAsset("p", catalogKey, blobs.put("beta"), strings.Repeat("e", 64), 1)
	r.holdListing(t.Context(), beta)
	if res := r.holdListing(t.Context(), testRelease("q", catalogKey, "2.0.0-beta", "beta", beta.ID)); !res.broadcast {
		t.Fatalf("other channel: %+v", res)
	}
}

func TestPendingListingSupersede(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	older := &nostr.Event{ID: strings.Repeat("1", 64), PubKey: catalogKey, Kind: events.KindApp, CreatedAt: 1, Tags: nostr.Tags{{"d", testApp}}}
	newer := &nostr.Event{ID: strings.Repeat("2", 64), PubKey: catalogKey, Kind: events.KindApp, CreatedAt: 2, Tags: nostr.Tags{{"d", testApp}}}
	r.holdListing(t.Context(), older)
	r.holdListing(t.Context(), newer)
	if kept, err := pendingHas(t.Context(), r.store, older.ID); err != nil || kept {
		t.Fatalf("older pending: %v %v", kept, err)
	}
	if kept, err := pendingHas(t.Context(), r.store, newer.ID); err != nil || !kept {
		t.Fatalf("newer pending: %v %v", kept, err)
	}
}

func TestDeleteDropsPendingListing(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	app, _, _ := testTriple("d", ownerKey, strings.Repeat("f", 64), strings.Repeat("e", 64), 1)
	r.holdListing(t.Context(), app)
	kind5 := &nostr.Event{
		ID: strings.Repeat("9", 64), PubKey: ownerKey, Kind: nostr.KindDeletion,
		Tags: nostr.Tags{{"e", app.ID}},
	}
	if err := r.handleDelete(t.Context(), kind5); err != nil {
		t.Fatal(err)
	}
	if kept, err := pendingHas(t.Context(), r.store, app.ID); err != nil || kept {
		t.Fatalf("pending app remains: %v %v", kept, err)
	}
	if !slotHas(t, r.store, kind5.ID) {
		t.Fatal("kind 5 was not stored")
	}
}

// promoteIndexer promotes a catalog-key listing for testApp.
func promoteIndexer(t *testing.T, r *T, x, cert string, extra ...nostr.Tag) {
	t.Helper()
	app, release, asset := testTriple("i", catalogKey, x, cert, 5)
	asset.Tags = append(asset.Tags, extra...)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	if res := r.holdListing(t.Context(), release); !res.broadcast {
		t.Fatalf("indexer listing: %+v", res)
	}
}

// drainReads runs queued APK reads and reconciles until nothing is queued.
func drainReads(t *testing.T, r *T) {
	t.Helper()
	for {
		for len(r.apks.jobs) > 0 {
			job := <-r.apks.jobs
			err := r.readAPKJob(t.Context(), job)
			r.apks.done(job.blob, err != nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := r.reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(r.apks.jobs) == 0 {
			return
		}
	}
}

// readChains reads an APK by its body.
func readChains(t *testing.T, chains map[string]signing) func(io.ReadSeeker) (signing, error) {
	return func(r io.ReadSeeker) (signing, error) {
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		chain, ok := chains[string(body)]
		if !ok {
			return nil, fmt.Errorf("not an apk: %q", body)
		}
		return chain, nil
	}
}

func pinned(t *testing.T, r *T) string {
	t.Helper()
	pin, _, err := appCertificate(t.Context(), r.store, catalogKey, testApp)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// testKey is an APK signing key and its self-signed certificate.
type testKey struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	hash string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Unix(0, 0), NotAfter: time.Now().AddDate(10, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{key: key, cert: cert, hash: certHash(cert)}
}

// proof is an active kind 30509 for pubkey, signed by the certificate key.
func (k testKey) proof(t *testing.T, n, pubkey, delegate string) *nostr.Event {
	t.Helper()
	created := time.Now().Unix()
	expiry := created + 365*24*3600
	msg := fmt.Sprintf("Verifying at %d until %d that I control the following Nostr public key: %s", created, expiry, pubkey)
	sum := sha256.Sum256([]byte(msg))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	tags := nostr.Tags{
		{"d", k.hash},
		{"signature", base64.StdEncoding.EncodeToString(sig)},
		{"expiry", strconv.FormatInt(expiry, 10)},
	}
	if delegate != "" {
		tags = append(tags, nostr.Tag{"delegation", delegate})
	}
	return &nostr.Event{
		ID: strings.Repeat(n, 64), PubKey: pubkey, Kind: events.KindIdentityProof,
		CreatedAt: nostr.Timestamp(created), Tags: tags,
	}
}

func testRelay(t *testing.T, blobs Blossom) *T {
	t.Helper()
	return &T{
		store:   newSlotDB(t),
		blossom: blobs,
		config:  Config{Info: Info{Pubkey: catalogKey}, RemovePendingAfter: time.Hour},
		apks:    newAPKReader(),
		readAPK: func(io.ReadSeeker) (signing, error) {
			t.Fatal("apk was read")
			return nil, errors.New("read")
		},
	}
}

func testTriple(n, pub, blob, cert string, code int) (app, release, asset *nostr.Event) {
	asset = testAsset(n, pub, blob, cert, code)
	app = &nostr.Event{
		ID: n + strings.Repeat("2", 63), PubKey: pub, Kind: events.KindApp, CreatedAt: 10,
		Tags: nostr.Tags{{"d", testApp}, {"name", "Example"}, {"f", "android-arm64-v8a"}},
	}
	release = testRelease(n, pub, "1.0.0", "main", asset.ID)
	return app, release, asset
}

func testAsset(n, pub, blob, cert string, code int) *nostr.Event {
	return &nostr.Event{
		ID: n + strings.Repeat("1", 63), PubKey: pub, Kind: events.KindAsset, CreatedAt: 10,
		Tags: nostr.Tags{
			{"i", testApp},
			{"x", blob},
			{"version", "1.0.0"},
			{"f", "android-arm64-v8a"},
			{"version_code", strconv.Itoa(code)},
			{"apk_certificate_hash", cert},
		},
	}
}

func testRelease(n, pub, version, channel, assetID string) *nostr.Event {
	return &nostr.Event{
		ID: n + strings.Repeat("3", 63), PubKey: pub, Kind: events.KindRelease, CreatedAt: 10,
		Tags: nostr.Tags{
			{"a", "32267:" + pub + ":" + testApp},
			{"i", testApp},
			{"version", version},
			{"d", testApp + "@" + version},
			{"c", channel},
			{"e", assetID},
			{"f", "android-arm64-v8a"},
		},
	}
}

type fakeBlossom struct {
	bodies   map[string][]byte
	opened   []string
	promoted []string
}

func newFakeBlossom() *fakeBlossom {
	return &fakeBlossom{bodies: map[string][]byte{}}
}

// put stores body and returns its sha256.
func (f *fakeBlossom) put(body string) string {
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	f.bodies[hash] = []byte(body)
	return hash
}

func (f *fakeBlossom) Has(_ context.Context, hash blossom.Hash) (bool, error) {
	_, ok := f.bodies[hash.Hex()]
	return ok, nil
}

func (f *fakeBlossom) Open(_ context.Context, hash blossom.Hash) (io.ReadCloser, error) {
	f.opened = append(f.opened, hash.Hex())
	body, ok := f.bodies[hash.Hex()]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeBlossom) Promote(_ context.Context, hash blossom.Hash) error {
	if _, ok := f.bodies[hash.Hex()]; ok {
		f.promoted = append(f.promoted, hash.Hex())
	}
	return nil
}

func (f *fakeBlossom) Expire(context.Context, time.Time) (int, error) {
	return 0, nil
}
