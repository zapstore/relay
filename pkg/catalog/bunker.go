package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/nbd-wtf/go-nostr/nip44"
	"github.com/nbd-wtf/go-nostr/nip46"
)

// bunkerClient is a small NIP-46 client. It replaces go-nostr's BunkerClient,
// which has two faults that matter in a long signing run:
//   - RPC drops its "relay ready" signal when the socket is already open, so a
//     request the bunker did sign is reported as "couldn't connect to any relay".
//   - Its reply loop sends on a channel RPC has already closed, which panics when
//     a reply arrives after the caller gave up.
//
// Here a request's reply channel has a buffer of one and is never closed, a late
// reply is dropped, and only the bunker's key is accepted as the sender.
type bunkerClient struct {
	pool         *nostr.SimplePool
	cancel       context.CancelFunc
	relays       []string
	clientKey    string
	target       string
	conversation [32]byte
	shared       []byte
	listeners    sync.Map // request id -> chan nip46.Response
	serial       atomic.Uint64
	prefix       string
}

// newBunkerClient returns a client whose pool and subscription end when runCtx
// is cancelled. cancel must cancel runCtx.
func newBunkerClient(runCtx context.Context, cancel context.CancelFunc, clientKey, target string, relays []string) (*bunkerClient, error) {
	conversation, err := nip44.GenerateConversationKey(target, clientKey)
	if err != nil {
		return nil, fmt.Errorf("conversation key: %w", err)
	}
	shared, err := nip04.ComputeSharedSecret(target, clientKey)
	if err != nil {
		return nil, fmt.Errorf("conversation key: %w", err)
	}
	return &bunkerClient{
		pool:         nostr.NewSimplePool(runCtx),
		cancel:       cancel,
		relays:       relays,
		clientKey:    clientKey,
		target:       target,
		conversation: conversation,
		shared:       shared,
		prefix:       "ct-" + strconv.Itoa(rand.Intn(65536)),
	}, nil
}

// subscribe listens for replies. It returns once a relay has stored the
// subscription, because a reply can arrive as soon as a request is published.
func (c *bunkerClient) subscribe(ctx, runCtx context.Context) error {
	clientPub, err := nostr.GetPublicKey(c.clientKey)
	if err != nil {
		return err
	}
	since := nostr.Now()
	stored := make(chan struct{})
	events := c.pool.SubscribeManyNotifyEOSE(runCtx, c.relays, nostr.Filter{
		Tags:  nostr.TagMap{"p": []string{clientPub}},
		Kinds: []int{nostr.KindNostrConnect},
		Since: &since,
	}, stored)
	go func() {
		for ie := range events {
			if ie.Event == nil || ie.Kind != nostr.KindNostrConnect || ie.PubKey != c.target {
				continue
			}
			plain, err := nip44.Decrypt(ie.Content, c.conversation)
			if err != nil {
				plain, err = nip04.Decrypt(ie.Content, c.shared)
				if err != nil {
					continue
				}
			}
			var resp nip46.Response
			if json.Unmarshal([]byte(plain), &resp) != nil || resp.ID == "" {
				continue
			}
			waiter, ok := c.listeners.Load(resp.ID)
			if !ok {
				continue // the caller gave up
			}
			select {
			case waiter.(chan nip46.Response) <- resp:
			default:
			}
		}
	}()
	select {
	case <-stored:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handshake subscribes, connects, and returns the bunker's public key.
// Everything must finish within bunkerHandshakeTimeout.
func (c *bunkerClient) handshake(ctx, runCtx context.Context, target, secret string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, bunkerHandshakeTimeout)
	defer cancel()
	if err := c.subscribe(ctx, runCtx); err != nil {
		return "", err
	}
	if _, err := c.rpc(ctx, "connect", []string{target, secret}); err != nil {
		return "", err
	}
	pub, err := c.rpc(ctx, "get_public_key", nil)
	if err != nil {
		return "", err
	}
	if pub != target {
		return "", fmt.Errorf("public key %s does not match %s", pub, target)
	}
	return pub, nil
}

// rpc sends one request and waits for its reply.
func (c *bunkerClient) rpc(ctx context.Context, method string, params []string) (string, error) {
	id := c.prefix + "-" + strconv.FormatUint(c.serial.Add(1), 10)
	req, err := json.Marshal(nip46.Request{ID: id, Method: method, Params: params})
	if err != nil {
		return "", err
	}
	content, err := nip44.Encrypt(string(req), c.conversation)
	if err != nil {
		return "", fmt.Errorf("error encrypting request: %w", err)
	}
	evt := nostr.Event{
		Content:   content,
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindNostrConnect,
		Tags:      nostr.Tags{{"p", c.target}},
	}
	if err := evt.Sign(c.clientKey); err != nil {
		return "", fmt.Errorf("sign request: %w", err)
	}

	waiter := make(chan nip46.Response, 1)
	c.listeners.Store(id, waiter)
	defer c.listeners.Delete(id)

	if err := c.publish(ctx, evt); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case resp := <-waiter:
		if resp.Error != "" {
			return "", fmt.Errorf("response error: %s", resp.Error)
		}
		return resp.Result, nil
	}
}

// publish returns once one relay has accepted evt.
func (c *bunkerClient) publish(ctx context.Context, evt nostr.Event) error {
	errc := make(chan error, len(c.relays))
	for _, url := range c.relays {
		go func() {
			relay, err := c.pool.EnsureRelay(url)
			if err != nil {
				errc <- err
				return
			}
			errc <- relay.Publish(ctx, evt)
		}()
	}
	var errs []error
	for range c.relays {
		err := <-errc
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("publish to bunker: %w", errors.Join(errs...))
}

// signEvent has the bunker sign evt and checks what comes back.
func (c *bunkerClient) signEvent(ctx context.Context, evt *nostr.Event) error {
	resp, err := c.rpc(ctx, "sign_event", []string{evt.String()})
	if err != nil {
		return err
	}
	var signed nostr.Event
	if err := json.Unmarshal([]byte(resp), &signed); err != nil {
		return fmt.Errorf("bunker returned a bad event: %w", err)
	}
	if signed.ID != evt.ID || !signed.CheckID() {
		return errors.New("bunker returned an event with another id")
	}
	if signed.PubKey != c.target {
		return errors.New("bunker signed with another key")
	}
	if ok, err := signed.CheckSignature(); err != nil || !ok {
		return errors.New("bunker returned an invalid signature")
	}
	*evt = signed
	return nil
}

// close ends the subscription and the relay sockets.
// go-nostr ties each relay to context.Background, so pool.Close alone leaves
// the websockets open. Take the connections first, then cancel, so the
// subscription loop does not dial a replacement.
func (c *bunkerClient) close() {
	var conns []*nostr.Connection
	c.pool.Relays.Range(func(_ string, relay *nostr.Relay) bool {
		if relay != nil && relay.Connection != nil {
			conns = append(conns, relay.Connection)
		}
		return true
	})
	c.cancel()
	c.pool.Close("signer closed")
	for _, conn := range conns {
		_ = conn.Close()
	}
}
