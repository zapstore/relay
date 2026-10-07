package catalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/nbd-wtf/go-nostr/nip46"
)

const (
	// bunkerDialTimeout bounds a relay that never answers TCP.
	// A refused port returns as soon as the dial fails.
	bunkerDialTimeout = 2 * time.Second
	// bunkerHandshakeTimeout bounds the NIP-46 connect after a relay accepts TCP.
	// The pool context must outlive this; it is cancelled only when the handshake fails.
	bunkerHandshakeTimeout = 5 * time.Second
	// bunkerSignTimeout bounds one later sign when the bunker stops answering.
	bunkerSignTimeout = 5 * time.Second
)

const fixtureSK = "0000000000000000000000000000000000000000000000000000000000000001"

// localBunkerClientKey matches hotbox/internal/bunker.localClientKey.
const localBunkerClientKey = "0000000000000000000000000000000000000000000000000000000000000002"

// Signer signs catalog manifests. A local key signs directly. A bunker URL
// signs through NIP-46, including a loopback ws:// relay.
type Signer struct {
	PubKey string
	secret string
	bunker *nip46.BunkerClient
}

// ParseSigner reads a local signing key: 1, 64-char hex, or nsec.
func ParseSigner(signWith string) (Signer, error) {
	secret := strings.TrimSpace(signWith)
	if secret == "" {
		return Signer{}, fmt.Errorf("SIGN_WITH is empty")
	}
	if secret == "1" {
		secret = fixtureSK
	} else if strings.HasPrefix(secret, "nsec") {
		prefix, value, err := nip19.Decode(secret)
		if err != nil || prefix != "nsec" {
			return Signer{}, fmt.Errorf("invalid nsec SIGN_WITH")
		}
		sk, ok := value.(string)
		if !ok {
			return Signer{}, fmt.Errorf("invalid nsec SIGN_WITH")
		}
		secret = sk
	} else if !nostr.IsValid32ByteHex(secret) {
		return Signer{}, fmt.Errorf("SIGN_WITH must be 1, 64-char hex, nsec, or bunker://")
	}
	pub, err := nostr.GetPublicKey(secret)
	if err != nil {
		return Signer{}, err
	}
	return Signer{PubKey: pub, secret: secret}, nil
}

// OpenSigner reads SIGN_WITH. A bunker:// URL connects before returning.
// ctx must stay active for later Sign calls on that bunker.
func OpenSigner(ctx context.Context, signWith string) (Signer, error) {
	signWith = strings.TrimSpace(signWith)
	if strings.HasPrefix(signWith, "bunker://") {
		return openBunker(ctx, signWith)
	}
	return ParseSigner(signWith)
}

// Sign sets the signer pubkey and signs event.
func (s Signer) Sign(ctx context.Context, event *nostr.Event) error {
	if s.PubKey == "" {
		return fmt.Errorf("signer has no public key")
	}
	event.PubKey = s.PubKey
	if event.ID == "" {
		event.ID = event.GetID()
	} else if event.ID != event.GetID() {
		return fmt.Errorf("event id does not match its body")
	}
	if s.bunker != nil {
		signCtx, cancel := context.WithTimeout(ctx, bunkerSignTimeout)
		defer cancel()
		return s.bunker.SignEvent(signCtx, event)
	}
	return event.Sign(s.secret)
}

func openBunker(ctx context.Context, bunkerURL string) (signer Signer, err error) {
	target, relays, err := bunkerTarget(bunkerURL)
	if err != nil {
		return Signer{}, err
	}
	writeBlock("connect bunker")
	if err := dialRelays(ctx, relays); err != nil {
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	// ConnectBunker keeps this context for the pool. Cancel it only when the
	// handshake does not finish; a live signer needs it for later Sign calls.
	connectCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(bunkerHandshakeTimeout, cancel)
	defer func() {
		if err != nil {
			timer.Stop()
			cancel()
		}
	}()
	remote, err := nip46.ConnectBunker(connectCtx, localBunkerClientKey, bunkerURL, nil, func(string) {})
	if err != nil {
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	pub, err := remote.GetPublicKey(connectCtx)
	if err != nil {
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	if pub != target {
		return Signer{}, fmt.Errorf("bunker public key %s does not match %s", pub, target)
	}
	if !timer.Stop() {
		return Signer{}, fmt.Errorf("bunker: timed out")
	}
	return Signer{PubKey: pub, bunker: remote}, nil
}

// dialRelays returns as soon as one relay accepts TCP.
// Every relay refusing the connection fails immediately.
func dialRelays(ctx context.Context, relays []string) error {
	ctx, cancel := context.WithTimeout(ctx, bunkerDialTimeout)
	defer cancel()
	errc := make(chan error, len(relays))
	for _, relay := range relays {
		go func() {
			errc <- dialRelay(ctx, relay)
		}()
	}
	var errs []error
	for range relays {
		err := <-errc
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func dialRelay(ctx context.Context, relay string) error {
	parsed, err := url.Parse(relay)
	if err != nil {
		return err
	}
	if parsed.Host == "" {
		return fmt.Errorf("relay %q has no host", relay)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		return err
	}
	return conn.Close()
}

func bunkerTarget(bunkerURL string) (string, []string, error) {
	parsed, err := url.Parse(bunkerURL)
	if err != nil {
		return "", nil, fmt.Errorf("bunker: %w", err)
	}
	if parsed.Scheme != "bunker" {
		return "", nil, fmt.Errorf("bunker: expected bunker://")
	}
	if !nostr.IsValidPublicKey(parsed.Host) {
		return "", nil, fmt.Errorf("bunker: invalid signer pubkey")
	}
	relays := parsed.Query()["relay"]
	if len(relays) == 0 {
		return "", nil, fmt.Errorf("bunker: relay is required")
	}
	for _, relay := range relays {
		if !nostr.IsValidRelayURL(relay) {
			return "", nil, fmt.Errorf("bunker: relay must be ws:// or wss://")
		}
	}
	return parsed.Host, relays, nil
}
