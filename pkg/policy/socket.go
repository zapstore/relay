package policy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// BanFunc records a ban and removes that publisher's listings.
type BanFunc func(ctx context.Context, pub Publisher, reason string) error

// Listen serves one JSON command per connection on a Unix socket.
// Commands are suggestions, remove, ban, allow, disallow, and allowed.
func Listen(ctx context.Context, path string, store *Store, ban BanFunc) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				os.Remove(path)
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(conn, store, ban)
		}()
	}
}

type command struct {
	Op     string `json:"op"`
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type response struct {
	OK          bool         `json:"ok"`
	Error       string       `json:"error,omitempty"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	Allowed     []AllowedRow `json:"allowed,omitempty"`
}

func serveConn(conn net.Conn, store *Store, ban BanFunc) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, 1<<20))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		writeResp(conn, response{Error: "read failed"})
		return
	}
	var cmd command
	if err := json.Unmarshal(line, &cmd); err != nil {
		writeResp(conn, response{Error: "invalid json"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch cmd.Op {
	case "suggestions":
		rows, err := store.Suggestions(ctx)
		if err != nil {
			writeResp(conn, response{Error: "list failed"})
			return
		}
		writeResp(conn, response{OK: true, Suggestions: rows})
	case "remove":
		if err := store.RemoveSuggestion(ctx, cmd.ID); err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		writeResp(conn, response{OK: true})
	case "ban":
		if ban == nil {
			writeResp(conn, response{Error: "ban is not available"})
			return
		}
		pub, err := Normalize(cmd.Kind, cmd.ID)
		if err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		if err := ban(ctx, pub, cmd.Reason); err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		writeResp(conn, response{OK: true})
	case "allow":
		pub, err := Normalize(cmd.Kind, cmd.ID)
		if err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		if err := store.Allow(ctx, pub, cmd.Reason); err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		writeResp(conn, response{OK: true})
	case "disallow":
		if err := store.Disallow(ctx, cmd.Kind, cmd.ID); err != nil {
			writeResp(conn, response{Error: err.Error()})
			return
		}
		writeResp(conn, response{OK: true})
	case "allowed":
		rows, err := store.AllowedList(ctx)
		if err != nil {
			writeResp(conn, response{Error: "list failed"})
			return
		}
		writeResp(conn, response{OK: true, Allowed: rows})
	default:
		writeResp(conn, response{Error: "unknown op"})
	}
}

func writeResp(w io.Writer, resp response) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(resp)
}
