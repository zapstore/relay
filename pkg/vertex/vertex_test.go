package vertex

import (
	"context"
	"os"
	"testing"
)

const (
	stranger     = "34dc3acca6f7720af2948dc34a0aee4506b1366c29e8e88efab37c330e1132af"
	fran         = "726a1e261cc6474674e8285e3951b3bb139be9a773d1acf49dc868db861a1c11"
	pip          = "f683e87035f7ad4f44e0b98cfbd9537e16455a92cd38cefc4cb31db7557f5ef2"
	leakedMickey = "e4b67f9f7c0a1cce1c24ca9196f8e1446fcce17fdef5d5eb46a3929433ea4d91"
)

// The following tests require a Nostr secret key to sign requests to the Vertex DVM.
// Run them with:
//
// VERTEX_SECRET_KEY=<your_secret_key> go test
var config = NewConfig()

func requireVertex(t *testing.T) {
	t.Helper()
	config.SecretKey = os.Getenv("VERTEX_SECRET_KEY")
	if config.SecretKey == "" {
		t.Skip("VERTEX_SECRET_KEY is not set")
	}
}

func TestAllowZeroThreshold(t *testing.T) {
	client := NewClient(config)
	client.config.Algorithm = Algorithm{Sort: SortGlobal, Threshold: 0.0}
	allow, _, err := client.Allow(context.Background(), stranger)
	if err != nil {
		t.Fatal(err)
	}
	if !allow {
		t.Fatal("expected allow")
	}
}

func TestAllow(t *testing.T) {
	requireVertex(t)
	tests := []struct {
		name      string
		pubkey    string
		algorithm Algorithm
		want      bool
	}{
		{
			name:      "stranger below threshold",
			pubkey:    stranger,
			algorithm: Algorithm{Sort: SortGlobal, Threshold: 0.0000001},
			want:      false,
		},
		{
			name:      "famous leaked key",
			pubkey:    leakedMickey,
			algorithm: Algorithm{Sort: SortGlobal, Threshold: 0.0000001},
			want:      false,
		},
		{
			name:      "known pubkey above threshold",
			pubkey:    fran,
			algorithm: Algorithm{Sort: SortGlobal, Threshold: 0.0000001},
			want:      true,
		},
		{
			name:      "known pubkey above threshold (followers count)",
			pubkey:    fran,
			algorithm: Algorithm{Sort: SortFollowers, Threshold: 1000},
			want:      true,
		},
		{
			name:      "known pubkey above threshold (personalized)",
			pubkey:    fran,
			algorithm: Algorithm{Sort: SortPersonalized, Threshold: 0.0000001, Source: pip},
			want:      true,
		},
	}

	client := NewClient(config)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client.config.Algorithm = test.algorithm
			client.cache.Purge() // clear the cache

			allow, _, err := client.Allow(context.Background(), test.pubkey)
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if allow != test.want {
				t.Errorf("expected %v, got %v", test.want, allow)
			}
		})
	}
}

func TestCheckCredits(t *testing.T) {
	requireVertex(t)
	client := NewClient(config)
	res, err := client.CheckCredits(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	t.Logf("credits response:\n %s", res)
}
