package catalog

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
	"github.com/nbd-wtf/go-nostr/nip46"
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

// fakeBunker is a loopback NIP-46 relay and signer. Its first drop sign
// requests are never answered, and the next late are answered after delay,
// which is how a lost or slow reply looks to the client.
type fakeBunker struct {
	sk, pk string
	drop   int
	late   int
	delay  time.Duration
	seen   atomic.Int32
}

func (f *fakeBunker) serve(t *testing.T) string {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var mu sync.Mutex
		write := func(v any) {
			mu.Lock()
			defer mu.Unlock()
			_ = conn.WriteJSON(v)
		}
		sub := ""
		var queued []nostr.Event // like hotbox, hold replies until the client subscribes
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg []json.RawMessage
			if json.Unmarshal(raw, &msg) != nil || len(msg) < 2 {
				continue
			}
			var action string
			_ = json.Unmarshal(msg[0], &action)
			switch action {
			case "REQ":
				_ = json.Unmarshal(msg[1], &sub)
				for _, ev := range queued {
					write([]any{"EVENT", sub, ev})
				}
				queued = nil
				write([]any{"EOSE", sub})
			case "EVENT":
				var req nostr.Event
				if json.Unmarshal(msg[1], &req) != nil {
					continue
				}
				write([]any{"OK", req.ID, true, ""})
				resp, wait, ok := f.answer(t, req)
				switch {
				case !ok:
				case wait > 0:
					go func() {
						time.Sleep(wait)
						write([]any{"EVENT", sub, resp})
					}()
				case sub == "":
					queued = append(queued, resp)
				default:
					write([]any{"EVENT", sub, resp})
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	relay := "ws" + strings.TrimPrefix(srv.URL, "http") + "/token"
	return "bunker://" + f.pk + "?relay=" + url.QueryEscape(relay) + "&secret=once"
}

// answer returns the reply to req, how long to hold it, and false when it is dropped.
func (f *fakeBunker) answer(t *testing.T, req nostr.Event) (nostr.Event, time.Duration, bool) {
	t.Helper()
	none := nostr.Event{}
	key, err := nip44.GenerateConversationKey(req.PubKey, f.sk)
	if err != nil {
		t.Error(err)
		return none, 0, false
	}
	session := nip46.Session{PublicKey: f.pk, ConversationKey: key}
	call, err := session.ParseRequest(&req)
	if err != nil {
		t.Error(err)
		return none, 0, false
	}
	result := "ack"
	var wait time.Duration
	switch call.Method {
	case "get_public_key":
		result = f.pk
	case "sign_event":
		n := int(f.seen.Add(1))
		if n <= f.drop {
			return none, 0, false
		}
		if n <= f.drop+f.late {
			wait = f.delay
		}
		var ev nostr.Event
		if err := json.Unmarshal([]byte(call.Params[0]), &ev); err != nil {
			t.Error(err)
			return none, 0, false
		}
		if err := ev.Sign(f.sk); err != nil {
			t.Error(err)
			return none, 0, false
		}
		result = ev.String()
	}
	_, resp, err := session.MakeResponse(call.ID, req.PubKey, result, nil)
	if err != nil {
		t.Error(err)
		return none, 0, false
	}
	if err := resp.Sign(f.sk); err != nil {
		t.Error(err)
		return none, 0, false
	}
	return resp, wait, true
}

func fastSigns(t *testing.T) {
	t.Helper()
	old := bunkerSignTimeout
	bunkerSignTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bunkerSignTimeout = old })
}

func newFakeBunker(t *testing.T, drop, late int) (*fakeBunker, string) {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBunker{sk: sk, pk: pk, drop: drop, late: late, delay: 2 * bunkerSignTimeout}
	return f, f.serve(t)
}

func TestBunkerSignRetriesAfterNoAnswer(t *testing.T) {
	fastSigns(t)
	f, bunker := newFakeBunker(t, 1, 0)
	signer, err := OpenSigner(t.Context(), bunker)
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: 1, CreatedAt: 10, Content: "ok", Tags: nostr.Tags{}}
	if err := signer.Sign(t.Context(), &event); err != nil {
		t.Fatal(err)
	}
	if ok, err := event.CheckSignature(); err != nil || !ok || event.PubKey != f.pk {
		t.Fatalf("valid %v err %v pubkey %s", ok, err, event.PubKey)
	}
	if f.seen.Load() != 2 {
		t.Fatalf("bunker saw %d sign requests, want 2", f.seen.Load())
	}
}

// A reply that arrives after its caller gave up must be dropped. go-nostr's
// client panicked here with "send on closed channel".
func TestBunkerLateReplyIsDropped(t *testing.T) {
	fastSigns(t)
	f, bunker := newFakeBunker(t, 0, 3)
	signer, err := OpenSigner(t.Context(), bunker)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		event := nostr.Event{Kind: 1, CreatedAt: nostr.Timestamp(10 + i), Content: "ok", Tags: nostr.Tags{}}
		if err := signer.Sign(t.Context(), &event); err != nil {
			t.Fatal(err)
		}
		if ok, err := event.CheckSignature(); err != nil || !ok {
			t.Fatalf("valid %v err %v", ok, err)
		}
	}
	time.Sleep(3 * bunkerSignTimeout) // let every late reply land
	if f.seen.Load() < 5 {
		t.Fatalf("bunker saw %d sign requests", f.seen.Load())
	}
}

func TestBunkerSignGivesUpAfterAttempts(t *testing.T) {
	fastSigns(t)
	_, bunker := newFakeBunker(t, bunkerSignAttempts, 0)
	signer, err := OpenSigner(t.Context(), bunker)
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: 1, CreatedAt: 10, Content: "ok", Tags: nostr.Tags{}}
	err = signer.Sign(t.Context(), &event)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
}

func bunkerURL(addr string) string {
	pk := strings.Repeat("ab", 32)
	relay := "ws://" + addr + "/token"
	return "bunker://" + pk + "?relay=" + url.QueryEscape(relay) + "&secret=once"
}
