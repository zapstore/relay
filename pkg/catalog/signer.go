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
)

const (
	// bunkerDialTimeout bounds a relay that never answers TCP.
	// A refused port returns as soon as the dial fails.
	bunkerDialTimeout = 2 * time.Second
	// bunkerHandshakeTimeout bounds the NIP-46 connect after a relay accepts TCP.
	// The pool context must outlive this; it is cancelled only when the handshake fails.
	bunkerHandshakeTimeout = 5 * time.Second
	// bunkerSignAttempts is how many times one sign is tried before it fails.
	bunkerSignAttempts = 4
)

// bunkerSignTimeout bounds one later sign when the bunker stops answering.
// It is a variable so tests can shorten it.
var bunkerSignTimeout = 5 * time.Second

const fixtureSK = "0000000000000000000000000000000000000000000000000000000000000001"

// localBunkerClientKey matches hotbox/internal/bunker.localClientKey.
const localBunkerClientKey = "0000000000000000000000000000000000000000000000000000000000000002"

// Signer signs catalog manifests. A local key signs directly. A bunker URL
// signs through NIP-46, including a loopback ws:// relay.
type Signer struct {
	PubKey string
	secret string
	bunker *bunkerClient
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
		return s.signBunker(ctx, event)
	}
	return event.Sign(s.secret)
}

// signBunker retries a sign the bunker did not answer in time. The event id is
// fixed, so signing it again is harmless. A rejection, an encryption error, or
// a cancelled ctx is not retried.
func (s Signer) signBunker(ctx context.Context, event *nostr.Event) error {
	var err error
	for attempt := 1; attempt <= bunkerSignAttempts; attempt++ {
		signCtx, cancel := context.WithTimeout(ctx, bunkerSignTimeout)
		err = s.bunker.signEvent(signCtx, event)
		timedOut := signCtx.Err() != nil
		cancel()
		if err == nil {
			return nil
		}
		if !timedOut || ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf("bunker did not answer after %d attempts: %w", bunkerSignAttempts, err)
}

func openBunker(ctx context.Context, bunkerURL string) (Signer, error) {
	target, relays, err := bunkerTarget(bunkerURL)
	if err != nil {
		return Signer{}, err
	}
	parsed, err := url.Parse(bunkerURL)
	if err != nil {
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	writeBlock("connect bunker")
	if err := dialRelays(ctx, relays); err != nil {
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	// The pool and its subscription outlive the handshake, so later Sign calls
	// use them. They end with ctx, or here when the handshake fails.
	runCtx, cancel := context.WithCancel(ctx)
	client, err := newBunkerClient(runCtx, cancel, localBunkerClientKey, target, relays)
	if err != nil {
		cancel()
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	pub, err := client.handshake(ctx, runCtx, target, parsed.Query().Get("secret"))
	if err != nil {
		client.close()
		return Signer{}, fmt.Errorf("bunker: %w", err)
	}
	return Signer{PubKey: pub, bunker: client}, nil
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
