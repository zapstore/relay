package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSiteFromEnv(t *testing.T) {
	saved := siteDefaults
	saved.RelayBinary = "/opt/zapstore/bin/relay"
	t.Setenv("LAUNCH_RELAY_BINARY", "/srv/relay/relay")
	t.Setenv("LAUNCH_RELAY_UNIT", "  ")
	got := siteFromEnv(saved)
	if got.RelayBinary != "/srv/relay/relay" {
		t.Fatalf("binary = %s", got.RelayBinary)
	}
	if got.RelayUnit != saved.RelayUnit {
		t.Fatalf("unit = %s", got.RelayUnit)
	}
	if got.RelayEnv != siteDefaults.RelayEnv {
		t.Fatalf("env file = %s", got.RelayEnv)
	}
}

func TestResolveSocket(t *testing.T) {
	data := "/var/lib/relay"
	if got := resolveSocket("", "", data); got != filepath.Join(data, "relay.sock") {
		t.Fatalf("default = %s", got)
	}
	if got := resolveSocket("/from/env.sock", "/from/unit.sock", data); got != "/from/env.sock" {
		t.Fatalf("env = %s", got)
	}
	if got := resolveSocket("  ", "/from/unit.sock", data); got != "/from/unit.sock" {
		t.Fatalf("unit = %s", got)
	}
	t.Setenv("LAUNCH_RELAY_SOCKET", "/from/override.sock")
	if got := resolveSocket("/from/env.sock", "/from/unit.sock", data); got != "/from/override.sock" {
		t.Fatalf("override = %s", got)
	}
}

func TestSocketFromUnit(t *testing.T) {
	text := `
[Service]
Environment=DATA_DIR=/var/lib/relay
Environment=RELAY_SOCKET_PATH=/run/old.sock
Environment="RELAY_SOCKET_PATH=/run/relay.sock"
`
	if got := socketFromUnit(text); got != "/run/relay.sock" {
		t.Fatalf("socket = %s", got)
	}
	quoted := `Environment=RELAY_SOCKET_PATH="/run/quoted.sock"`
	if got := socketFromUnit(quoted); got != "/run/quoted.sock" {
		t.Fatalf("quoted = %s", got)
	}
	if socketFromUnit("[Service]\nExecStart=/bin/relay\n") != "" {
		t.Fatal("found a socket in a unit that sets none")
	}
}

func TestWithin(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if !within(sub, dir) || !within(dir, dir) {
		t.Fatal("a path inside the data directory was allowed as a backup root")
	}
	if within(filepath.Dir(dir), dir) {
		t.Fatal("the parent directory was treated as inside")
	}
}

func TestPromptUnread(t *testing.T) {
	dir := t.TempDir()
	s := site{
		RelayEnv:   filepath.Join(dir, "relay.env"),
		IndexerEnv: filepath.Join(dir, "indexer.env"),
	}
	in := strings.NewReader("/data/relay\n127.0.0.1:3334\n/run/relay.sock\n/data/indexer\n")
	var out strings.Builder
	if err := s.promptUnread(t.Context(), newTerm(in, &out)); err != nil {
		t.Fatal(err)
	}
	if s.RelayData != "/data/relay" || s.RelayAddr != "127.0.0.1:3334" || s.SocketPath != "/run/relay.sock" || s.IndexerData != "/data/indexer" {
		t.Fatalf("prompted site = %+v", s)
	}
	if !strings.Contains(out.String(), "Cannot read") {
		t.Fatal(out.String())
	}
}

func TestRelayPathsFileWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.env")
	if err := os.WriteFile(path, []byte("DATA_DIR=/from/file\nRELAY_ADDRESS=localhost:9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := site{RelayEnv: path, RelayData: "/from/prompt", RelayAddr: "localhost:1"}
	data, addr, _, err := s.relayPaths()
	if err != nil {
		t.Fatal(err)
	}
	if data != "/from/file" || addr != "localhost:9" {
		t.Fatalf("data=%s addr=%s", data, addr)
	}
}

func TestRelayPathsFromPrompt(t *testing.T) {
	s := site{
		RelayEnv:   filepath.Join(t.TempDir(), "relay.env"),
		RelayData:  "/var/lib/zapstore/data",
		RelayAddr:  "localhost:3334",
		SocketPath: "/run/relay.sock",
	}
	data, addr, sock, err := s.relayPaths()
	if err != nil {
		t.Fatal(err)
	}
	if data != s.RelayData || addr != s.RelayAddr || sock != s.SocketPath {
		t.Fatalf("data=%s addr=%s sock=%s", data, addr, sock)
	}
}

func TestUnsetPubkeyUsesSigner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.env")
	body := "SIGN_WITH=1\nCATALOG_BUNDLE_INTERVAL=0\nBLOSSOM_PRUNE_INTERVAL=0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	key, signer, err := checkRelayEnv(t.Context(), path, relayEnvWant{})
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || key != signer.PubKey {
		t.Fatalf("key=%s signer=%s", key, signer.PubKey)
	}
	if _, _, err := checkRelayEnv(t.Context(), path, relayEnvWant{key: oldKey}); err == nil {
		t.Fatal("accepted a signer that is not 78ce…")
	}
}

func TestRedact(t *testing.T) {
	err := redact(fmt.Errorf("dial bunker://pubkey?relay=ws://127.0.0.1:1&secret=hunter2: refused"))
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "bunker://") {
		t.Fatal(err)
	}
	plain := errors.New("SIGN_WITH signs as aa, want bb")
	if redact(plain) != plain {
		t.Fatal("redact changed an error with no secret")
	}
}
