package tests

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/control"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

// listingRig is the relay and Blossom running in process, with the catalog
// key's secret to sign with and its control socket to publish on.
type listingRig struct {
	sk, pk  string
	socket  string
	blobURL string
	hash    string
	apk     []byte
	blossom string
}

func startListingRig(t *testing.T) listingRig {
	t.Helper()
	dir := t.TempDir()
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	relayDB, err := relay.NewDB(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relayDB.Close() })
	blossomDB, err := blossom.NewDB(filepath.Join(dir, "blossom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blossomDB.Close() })

	bcfg := blossom.NewConfig()
	bcfg.Hostname = "localhost"
	bcfg.Address = freeAddr(t)
	bcfg.Dir = filepath.Join(dir, "blossom")
	rcfg := relay.NewConfig()
	rcfg.Info.Pubkey = pk
	rcfg.Address = freeAddr(t)
	rcfg.Hostname = "localhost"
	limiter := rate.NewLimiter(rate.NewConfig())

	r, err := relay.Setup(rcfg, limiter, nil, nil, relayDB, blossom.NewBytes(bcfg, blossomDB), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := blossom.Setup(bcfg, limiter, nil, nil, blossomDB, r, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// A Unix socket path is limited to about 100 bytes, which a subtest's temp dir exceeds.
	short, err := os.MkdirTemp("", "rg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	socket := filepath.Join(short, "r.sock")
	go r.StartAndServe(ctx, rcfg.Address, nil)
	go b.StartAndServe(ctx, bcfg.Address)
	sock := &control.Server{Relay: pk, Nostr: r.ServeNostr}
	go sock.Listen(ctx, socket)

	apk, err := os.ReadFile(filepath.Join("..", "pkg", "relay", "testdata", "test-release.apk"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(apk)
	hash := hex.EncodeToString(sum[:])
	waitFor(t, "http://"+bcfg.Address+"/"+hash)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control socket did not start: %v", err)
		}
	}
	return listingRig{
		sk: sk, pk: pk, socket: socket, hash: hash, apk: apk, blossom: bcfg.Address,
		blobURL: "http://" + bcfg.Address + "/" + hash + ".apk",
	}
}

// events returns the app, the APK asset, a desktop asset, and a release that
// names both assets.
func (g listingRig) events(t *testing.T) (app, apk, desktop, release nostr.Event) {
	t.Helper()
	apk = sign(t, g.sk, nostr.Event{Kind: 3063, Tags: nostr.Tags{
		{"i", "com.example.app"}, {"x", g.hash}, {"version", "1.0.0"}, {"version_code", "1"},
		{"f", "android-arm64-v8a"}, {"m", "application/vnd.android.package-archive"},
		{"apk_certificate_hash", testReleaseSigner},
	}})
	desktop = sign(t, g.sk, nostr.Event{Kind: 3063, Tags: nostr.Tags{
		{"i", "com.example.app"}, {"x", strings.Repeat("d", 64)}, {"version", "1.0.0"}, {"version_code", "1"},
		{"f", "linux-x86_64"}, {"m", "application/x-executable"},
		{"apk_certificate_hash", testReleaseSigner},
	}})
	app = sign(t, g.sk, nostr.Event{Kind: 32267, Tags: nostr.Tags{
		{"d", "com.example.app"}, {"name", "Example"}, {"f", "android-arm64-v8a"},
	}})
	release = sign(t, g.sk, nostr.Event{Kind: 30063, Tags: nostr.Tags{
		{"a", "32267:" + g.pk + ":com.example.app"}, {"i", "com.example.app"}, {"version", "1.0.0"},
		{"d", "com.example.app@1.0.0"}, {"c", "main"},
		{"e", apk.ID}, {"e", desktop.ID}, {"f", "android-arm64-v8a"}, {"f", "linux-x86_64"},
	}})
	return app, apk, desktop, release
}

// send publishes one event on the control socket and returns the OK reply.
func (g listingRig) send(t *testing.T, event nostr.Event) (ok bool, msg string) {
	t.Helper()
	conn, err := net.Dial("unix", g.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := json.Marshal([]any{"EVENT", event})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var parts []any
	if err := json.Unmarshal(reply, &parts); err != nil || len(parts) < 4 {
		t.Fatalf("kind %d: bad reply %s", event.Kind, reply)
	}
	ok, _ = parts[2].(bool)
	msg, _ = parts[3].(string)
	return ok, msg
}

func (g listingRig) mustSend(t *testing.T, event nostr.Event) string {
	t.Helper()
	ok, msg := g.send(t, event)
	if !ok {
		t.Fatalf("kind %d was rejected: %s", event.Kind, msg)
	}
	return msg
}

// served waits for the APK to be served, which happens when its listing is promoted.
func (g listingRig) served(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for status(t, http.MethodGet, g.blobURL) != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("the APK was not served: the listing was not promoted")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRejectedAssetDoesNotHoldTheListing publishes a release that names an
// arm64 APK and a desktop asset. The relay rejects the desktop asset, and the
// APK is listed in either order of arrival.
func TestRejectedAssetDoesNotHoldTheListing(t *testing.T) {
	t.Run("asset first", func(t *testing.T) {
		g := startListingRig(t)
		upload(t, "http://"+g.blossom+"/upload", g.sk, g.hash, g.apk)
		app, apk, desktop, release := g.events(t)

		ok, msg := g.send(t, desktop)
		if ok || !strings.Contains(msg, "platform must include android-arm64-v8a") {
			t.Fatalf("desktop asset: ok=%v %q", ok, msg)
		}
		g.mustSend(t, app)
		g.mustSend(t, apk)
		msg = g.mustSend(t, release)
		if !strings.Contains(msg, "1 of 2 assets were rejected") {
			t.Fatalf("release reply does not report the skipped asset: %q", msg)
		}
		g.served(t)
	})

	t.Run("release first", func(t *testing.T) {
		g := startListingRig(t)
		upload(t, "http://"+g.blossom+"/upload", g.sk, g.hash, g.apk)
		app, apk, desktop, release := g.events(t)

		g.mustSend(t, app)
		msg := g.mustSend(t, release)
		if !strings.Contains(msg, "pending") || !strings.Contains(msg, "waiting for 2 of 2 assets") {
			t.Fatalf("release reply does not say what it waits for: %q", msg)
		}
		g.mustSend(t, apk)
		if status(t, http.MethodGet, g.blobURL) == http.StatusOK {
			t.Fatal("the listing was promoted while an asset was still expected")
		}
		if ok, _ := g.send(t, desktop); ok {
			t.Fatal("the desktop asset was accepted")
		}
		g.served(t)
	})

	t.Run("every asset rejected", func(t *testing.T) {
		g := startListingRig(t)
		app, _, desktop, release := g.events(t)
		release = sign(t, g.sk, nostr.Event{Kind: 30063, Tags: nostr.Tags{
			{"a", "32267:" + g.pk + ":com.example.app"}, {"i", "com.example.app"}, {"version", "1.0.0"},
			{"d", "com.example.app@1.0.0"}, {"c", "main"}, {"e", desktop.ID}, {"f", "android-arm64-v8a"},
		}})
		if ok, _ := g.send(t, desktop); ok {
			t.Fatal("the desktop asset was accepted")
		}
		g.mustSend(t, app)
		ok, msg := g.send(t, release)
		if ok || !strings.Contains(msg, "no asset of the release was accepted") {
			t.Fatalf("release: ok=%v %q", ok, msg)
		}
	})
}
