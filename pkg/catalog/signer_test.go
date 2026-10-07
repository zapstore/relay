package catalog

import (
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func TestParseSignerFixtureSigns(t *testing.T) {
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: 1, CreatedAt: 10, Content: "ok", Tags: nostr.Tags{}}
	if err := signer.Sign(t.Context(), &event); err != nil {
		t.Fatal(err)
	}
	ok, err := event.CheckSignature()
	if err != nil || !ok || event.PubKey != signer.PubKey {
		t.Fatalf("valid %v err %v pubkey %s", ok, err, event.PubKey)
	}
}

func TestBunkerTargetAllowsLoopbackWS(t *testing.T) {
	pk := strings.Repeat("ab", 32)
	raw := "bunker://" + pk + "?relay=ws%3A%2F%2F127.0.0.1%3A9%2Ftoken&secret=once"
	got, relays, err := bunkerTarget(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != pk || len(relays) != 1 || relays[0] != "ws://127.0.0.1:9/token" {
		t.Fatalf("pubkey %s relays %v", got, relays)
	}
	if _, _, err := bunkerTarget("bunker://" + pk + "?relay=https%3A%2F%2F127.0.0.1"); err == nil {
		t.Fatal("https relay was accepted")
	}
	if _, _, err := bunkerTarget("bunker://" + pk); err == nil {
		t.Fatal("missing relay was accepted")
	}
}

func TestOpenBunkerRefusedFailsImmediately(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = OpenSigner(t.Context(), bunkerURL(addr))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed > time.Second {
		t.Fatalf("took %s: %v", elapsed, err)
	}
}

func TestOpenBunkerSilentRelayTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-t.Context().Done()
				conn.Close()
			}()
		}
	}()
	start := time.Now()
	_, err = OpenSigner(t.Context(), bunkerURL(ln.Addr().String()))
	elapsed := time.Since(start)
	ln.Close()
	<-done
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed > bunkerHandshakeTimeout+2*time.Second {
		t.Fatalf("took %s: %v", elapsed, err)
	}
}

func bunkerURL(addr string) string {
	pk := strings.Repeat("ab", 32)
	relay := "ws://" + addr + "/token"
	return "bunker://" + pk + "?relay=" + url.QueryEscape(relay) + "&secret=once"
}
