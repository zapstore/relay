package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

func TestPubkeyHex(t *testing.T) {
	secret := nostr.GeneratePrivateKey()
	hexKey, err := nostr.GetPublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	npub, err := nip19.EncodePublicKey(hexKey)
	if err != nil {
		t.Fatal(err)
	}

	got, err := PubkeyHex(npub)
	if err != nil {
		t.Fatal(err)
	}
	if got != hexKey {
		t.Fatalf("PubkeyHex(npub) = %q, want %q", got, hexKey)
	}

	got, err = PubkeyHex("  " + strings.ToUpper(npub) + "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != hexKey {
		t.Fatalf("PubkeyHex(upper npub) = %q, want %q", got, hexKey)
	}

	got, err = PubkeyHex(hexKey)
	if err != nil || got != hexKey {
		t.Fatalf("PubkeyHex(hex) = %q, %v", got, err)
	}
	got, err = PubkeyHex("")
	if err != nil || got != "" {
		t.Fatalf("PubkeyHex(empty) = %q, %v", got, err)
	}
	if _, err := PubkeyHex("npub1notakey"); err == nil {
		t.Fatal("PubkeyHex(invalid npub) = nil, want error")
	}
}
