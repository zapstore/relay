package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/control"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

// TestBufferZone runs the relay and Blossom in process. An upload is not
// served until the catalog key's listing names it.
func TestBufferZone(t *testing.T) {
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
	socket := filepath.Join(dir, "relay.sock")
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
	blobURL := "http://" + bcfg.Address + "/" + hash + ".apk"
	waitFor(t, "http://"+bcfg.Address+"/"+hash)

	upload(t, "http://"+bcfg.Address+"/upload", sk, hash, apk)
	if code := status(t, http.MethodHead, blobURL); code != http.StatusNotFound {
		t.Fatalf("pending HEAD = %d, want 404", code)
	}

	assetEvent := sign(t, sk, nostr.Event{Kind: 3063, Tags: nostr.Tags{
		{"i", "com.example.app"}, {"x", hash}, {"version", "1.0.0"}, {"version_code", "1"},
		{"f", "android-arm64-v8a"}, {"m", "application/vnd.android.package-archive"},
		{"apk_certificate_hash", testReleaseSigner},
	}})
	app := sign(t, sk, nostr.Event{Kind: 32267, Tags: nostr.Tags{
		{"d", "com.example.app"}, {"name", "Example"}, {"f", "android-arm64-v8a"},
	}})
	release := sign(t, sk, nostr.Event{Kind: 30063, Tags: nostr.Tags{
		{"a", "32267:" + pk + ":com.example.app"}, {"i", "com.example.app"}, {"version", "1.0.0"},
		{"d", "com.example.app@1.0.0"}, {"c", "main"}, {"e", assetEvent.ID}, {"f", "android-arm64-v8a"},
	}})
	for _, event := range []nostr.Event{app, assetEvent, release} {
		publishSocket(t, socket, event)
	}

	// The catalog-key listing is stored as it arrives, and that serves the file.
	deadline := time.Now().Add(5 * time.Second)
	for status(t, http.MethodGet, blobURL) != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("promoted GET is not 200")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testReleaseSigner is the certificate sha256 of test-release.apk.
const testReleaseSigner = "d3bd3d00681ef7da9d157961477304ff86086ba6002354dbf6a3dc5ea3f7266a"

func publishSocket(t *testing.T, path string, event nostr.Event) {
	t.Helper()
	conn, err := net.Dial("unix", path)
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
	var ok []any
	if err := json.Unmarshal(reply, &ok); err != nil || len(ok) < 3 || ok[2] != true {
		t.Fatalf("kind %d: %s", event.Kind, reply)
	}
}

func upload(t *testing.T, url, sk, hash string, body []byte) {
	t.Helper()
	auth := sign(t, sk, nostr.Event{Kind: 24242, Tags: nostr.Tags{
		{"t", "upload"}, {"x", hash}, {"expiration", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
	}})
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
	req.Header.Set("Content-Type", "application/vnd.android.package-archive")
	req.Header.Set("Content-Digest", hash)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d %s", res.StatusCode, res.Header.Get("X-Reason"))
	}
}

func sign(t *testing.T, sk string, event nostr.Event) nostr.Event {
	t.Helper()
	event.CreatedAt = nostr.Now()
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return event
}

func status(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Head(url); err == nil {
			res.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not start", url)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
