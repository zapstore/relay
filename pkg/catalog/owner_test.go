package catalog

import (
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestC1Owner(t *testing.T) {
	cert := repeatHex("ab", 32)
	at := time.Unix(40, 0)
	older := proof(t, "11", cert, "01", 10, "100")
	revoked := nostr.Event{
		Kind:      events.KindIdentityProof,
		PubKey:    "22",
		ID:        "02",
		CreatedAt: 20,
		Tags:      nostr.Tags{{"d", cert}, {"revoked"}},
	}
	expired := proof(t, "33", cert, "03", 30, "25")
	owner, _, ok := c1Publisher([]Proof{
		{Pubkey: older.PubKey, D: cert, Event: older},
		{Pubkey: revoked.PubKey, D: cert, Event: revoked},
		{Pubkey: expired.PubKey, D: cert, Event: expired},
	}, cert, at)
	if !ok || owner != older.PubKey {
		t.Fatalf("owner = %q ok=%v", owner, ok)
	}

	newer := proof(t, "44", cert, "04", 50, "100")
	owner, _, ok = c1Publisher([]Proof{
		{Pubkey: older.PubKey, D: cert, Event: older},
		{Pubkey: newer.PubKey, D: cert, Event: newer},
	}, cert, time.Unix(60, 0))
	if !ok || owner != newer.PubKey {
		t.Fatalf("newest owner = %q ok=%v", owner, ok)
	}

	low := proof(t, "55", cert, "0a", 50, "100")
	high := proof(t, "66", cert, "0b", 50, "100")
	owner, _, ok = c1Publisher([]Proof{
		{Pubkey: high.PubKey, D: cert, Event: high},
		{Pubkey: low.PubKey, D: cert, Event: low},
	}, cert, time.Unix(60, 0))
	if !ok || owner != low.PubKey {
		t.Fatalf("tie owner = %q ok=%v", owner, ok)
	}

	replaced := proof(t, "22", cert, "11", 10, "100")
	revokedCurrent := nostr.Event{
		Kind:      events.KindIdentityProof,
		PubKey:    "22",
		ID:        "19",
		CreatedAt: 40,
		Tags:      nostr.Tags{{"d", cert}, {"revoked"}},
	}
	other := proof(t, "77", cert, "20", 30, "100")
	owner, _, ok = c1Publisher([]Proof{
		{Pubkey: replaced.PubKey, D: cert, Event: replaced},
		{Pubkey: revokedCurrent.PubKey, D: cert, Event: revokedCurrent},
		{Pubkey: other.PubKey, D: cert, Event: other},
	}, cert, at)
	if !ok || owner != other.PubKey {
		t.Fatalf("revoked replacement owner = %q ok=%v", owner, ok)
	}
	if _, _, ok := c1Publisher([]Proof{
		{Pubkey: replaced.PubKey, D: cert, Event: replaced},
		{Pubkey: revokedCurrent.PubKey, D: cert, Event: revokedCurrent},
	}, cert, at); ok {
		t.Fatal("revoked replacement still has an owner")
	}
}

func TestChooseListingSlots(t *testing.T) {
	cert := repeatHex("ab", 32)
	other := repeatHex("cd", 32)
	at := time.Unix(40, 0)
	catalogKey := "cc"
	owner := proof(t, "dd", cert, "01", 10, "100")
	owner.Tags = append(owner.Tags, nostr.Tag{"delegation", "ee"})
	proofs := []Proof{{Pubkey: owner.PubKey, D: cert, Event: owner}}

	indexer := slotListing(catalogKey, cert, "10", "2")
	developer := slotListing("dd", cert, "11", "2")
	chosen, ok := chooseListing([]Listing{indexer, developer}, proofs, at, catalogKey)
	if !ok || chosen.App.PubKey != "dd" {
		t.Fatalf("tie chose %q ok=%v", chosen.App.PubKey, ok)
	}

	delegate := slotListing("ee", cert, "12", "2")
	chosen, ok = chooseListing([]Listing{indexer, delegate}, proofs, at, catalogKey)
	if !ok || chosen.App.PubKey != "ee" {
		t.Fatalf("delegate tie chose %q ok=%v", chosen.App.PubKey, ok)
	}

	newerIndexer := slotListing(catalogKey, cert, "13", "3")
	chosen, ok = chooseListing([]Listing{newerIndexer, developer}, proofs, at, catalogKey)
	if !ok || chosen.App.PubKey != catalogKey {
		t.Fatalf("higher indexer chose %q ok=%v", chosen.App.PubKey, ok)
	}

	wrong := slotListing("dd", other, "14", "9")
	chosen, ok = chooseListing([]Listing{indexer, wrong}, proofs, at, catalogKey)
	if !ok || chosen.App.PubKey != catalogKey {
		t.Fatalf("wrong cert chose %q ok=%v", chosen.App.PubKey, ok)
	}

	if _, ok := chooseListing([]Listing{slotListing("ff", cert, "15", "1")}, proofs, at, catalogKey); ok {
		t.Fatal("stranger listing selected")
	}
}

func slotListing(pub, cert, id, code string) Listing {
	return Listing{
		AppID: "com.example.app",
		App: nostr.Event{
			Kind:   events.KindApp,
			PubKey: pub,
			ID:     id,
			Tags:   nostr.Tags{{"d", "com.example.app"}},
		},
		Assets: []nostr.Event{{
			Kind:   events.KindAsset,
			PubKey: pub,
			Tags: nostr.Tags{
				{"i", "com.example.app"},
				{"apk_certificate_hash", cert},
				{"version_code", code},
			},
		}},
	}
}

func proof(t *testing.T, pub, cert, id string, created int64, expiry string) nostr.Event {
	t.Helper()
	return nostr.Event{
		Kind:      events.KindIdentityProof,
		PubKey:    pub,
		ID:        id,
		CreatedAt: nostr.Timestamp(created),
		Tags:      nostr.Tags{{"d", cert}, {"expiry", expiry}},
	}
}

func repeatHex(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
