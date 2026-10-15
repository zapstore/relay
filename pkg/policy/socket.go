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
// The command is ban. readOnly rejects that command.
func Listen(ctx context.Context, path string, store *Store, ban BanFunc, readOnly bool) error {
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
			serveConn(conn, store, ban, readOnly)
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
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func serveConn(conn net.Conn, store *Store, ban BanFunc, readOnly bool) {
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
	case "ban":
		if readOnly {
			writeResp(conn, response{Error: "relay is read-only"})
			return
		}
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
	default:
		writeResp(conn, response{Error: "unknown op"})
	}
}

func writeResp(w io.Writer, resp response) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(resp)
}
