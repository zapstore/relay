package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// The relay closes a NIP-01 relay.sock connection after 30 seconds, and reads at most
// RELAY_MAX_MESSAGE_BYTES (default 500000) from one connection in total.
// Reconnect well before either.
const (
	socketReuse = 20 * time.Second
	socketBytes = 200_000
)

// nostrSocket is a NIP-01 client on relay.sock.
type nostrSocket struct {
	path   string
	conn   net.Conn
	r      *bufio.Reader
	opened time.Time
	sent   int
}

func (s *nostrSocket) Close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *nostrSocket) send(ctx context.Context, msg ...any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if s.conn != nil && (time.Since(s.opened) >= socketReuse || s.sent+len(raw) >= socketBytes) {
		s.Close()
	}
	if s.conn == nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", s.path)
		if err != nil {
			return fmt.Errorf("dial %s: %w", s.path, err)
		}
		s.conn, s.r, s.opened, s.sent = conn, bufio.NewReader(conn), time.Now(), 0
	}
	s.sent += len(raw) + 1
	if _, err := s.conn.Write(append(raw, '\n')); err != nil {
		s.Close()
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

func (s *nostrSocket) read() ([]json.RawMessage, string, error) {
	line, err := s.r.ReadBytes('\n')
	if err != nil {
		s.Close()
		return nil, "", fmt.Errorf("read %s: %w", s.path, err)
	}
	var msg []json.RawMessage
	var typ string
	if err := json.Unmarshal(line, &msg); err != nil || len(msg) == 0 || json.Unmarshal(msg[0], &typ) != nil {
		s.Close()
		return nil, "", fmt.Errorf("read %s: unexpected %q", s.path, line)
	}
	return msg, typ, nil
}

// publish sends one event and returns the relay's OK.
func (s *nostrSocket) publish(ctx context.Context, e nostr.Event) (bool, string, error) {
	if err := s.send(ctx, "EVENT", e); err != nil {
		return false, "", err
	}
	msg, typ, err := s.read()
	if err != nil {
		return false, "", err
	}
	if typ != "OK" || len(msg) < 4 {
		s.Close()
		return false, "", fmt.Errorf("event %s: relay answered %s", e.ID, msg)
	}
	var ok bool
	var reason string
	if err := json.Unmarshal(msg[2], &ok); err != nil {
		return false, "", err
	}
	if err := json.Unmarshal(msg[3], &reason); err != nil {
		return false, "", err
	}
	return ok, reason, nil
}

// query runs one REQ and returns the events up to EOSE.
func (s *nostrSocket) query(ctx context.Context, f nostr.Filter) ([]nostr.Event, error) {
	if err := s.send(ctx, "REQ", "launch", f); err != nil {
		return nil, err
	}
	var out []nostr.Event
	for {
		msg, typ, err := s.read()
		if err != nil {
			return nil, err
		}
		switch typ {
		case "EVENT":
			if len(msg) < 3 {
				return nil, fmt.Errorf("short EVENT")
			}
			var e nostr.Event
			if err := json.Unmarshal(msg[2], &e); err != nil {
				return nil, err
			}
			out = append(out, e)
		case "EOSE":
			return out, nil
		default:
			return nil, fmt.Errorf("REQ: relay answered %s", msg)
		}
	}
}

// nip11Pubkey reads the relay's NIP-11 pubkey.
func nip11Pubkey(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/nostr+json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Pubkey string `json:"pubkey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("NIP-11: %w", err)
	}
	return doc.Pubkey, nil
}

// waitRelay waits up to two minutes for NIP-11 to answer with key.
func waitRelay(ctx context.Context, t term, addr, key string) error {
	t.say("Waiting for the relay on %s", addr)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last error
	for {
		got, err := nip11Pubkey(ctx, addr)
		switch {
		case err == nil && got == key:
			t.say("NIP-11 pubkey is %s", key)
			return nil
		case err == nil:
			return fmt.Errorf("NIP-11 pubkey is %s, want %s", got, key)
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("relay did not answer: %w (%w)", last, context.Cause(ctx))
		case <-tick.C:
		}
	}
}

// waitSocket waits until path is a socket. Relay v2 creates it after it starts.
func waitSocket(ctx context.Context, t term, path string) error {
	t.say("Waiting for %s", path)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		info, err := os.Stat(path)
		switch {
		case err == nil && info.Mode()&os.ModeSocket != 0:
			return nil
		case err == nil:
			return fmt.Errorf("%s is not a socket", path)
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("relay socket %s did not appear: %w", path, last)
		case <-tick.C:
		}
	}
}
