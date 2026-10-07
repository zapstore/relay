package relay

import (
	"errors"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestCatalogKeyOnSocket(t *testing.T) {
	key := strings.Repeat("ab", 32)
	hook := catalogKeyOnSocket(key)
	if err := hook(nil, &nostr.Event{PubKey: key}); !errors.Is(err, errCatalogKeySocket) {
		t.Fatalf("catalog key: %v", err)
	}
	if err := hook(nil, &nostr.Event{PubKey: strings.Repeat("cd", 32)}); err != nil {
		t.Fatalf("other key: %v", err)
	}
}
