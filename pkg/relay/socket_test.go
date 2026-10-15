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

func TestReadOnlyEvent(t *testing.T) {
	if err := readOnlyEvent(nil, &nostr.Event{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	doc := nip11Doc(Info{}, true)
	if doc.Limitation == nil || !doc.Limitation.RestrictedWrites {
		t.Fatalf("limitation: %+v", doc.Limitation)
	}
	if nip11Doc(Info{}, false).Limitation != nil {
		t.Fatal("writes advertised as restricted")
	}
}
