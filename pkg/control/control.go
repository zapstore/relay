// Package control serves relay.sock, the only write path into the relay
// from another process. A read needs only access to the socket file. A write
// needs the relay's own Unix user, or an admin's signed command event.
package control

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/policy"
)

const (
	// connTimeout bounds one exchange. A ban deletes listings inside it.
	connTimeout   = 15 * time.Second
	writeTimeout  = 10 * time.Second
	sealTimeout   = 15 * time.Minute
	enrichTimeout = 15 * time.Minute
)

// ownUID is the user that may send unsigned writes, besides root.
var ownUID = os.Getuid

// SealRequest is one seal. HasFrom writes the bundle from that snapshot to
// the current one. Otherwise the next snapshot is sealed, limited to app ids
// containing Filter when it is set.
type SealRequest struct {
	Filter  string
	From    int64
	To      int64
	HasFrom bool
}

// SealFunc writes a bundle and returns the line shown on the job.
type SealFunc func(ctx context.Context, req SealRequest) (string, error)

// EnrichFunc enriches one app and returns the enricher report.
type EnrichFunc func(ctx context.Context, appID string) (string, error)

// Server answers relay.sock. Set the fields, then call Listen.
type Server struct {
	Store  *policy.Store
	DB     *sql.DB // relay.db; holds admin_log
	Ban    policy.BanFunc
	Seal   SealFunc
	Enrich EnrichFunc
	Data   string // the relay data directory: snapshots and bundles
	// Relay is the catalog pubkey. A signed command names it, so a command
	// signed for one relay does not run on another.
	Relay string
	// Admins may sign commands. Hex pubkeys.
	Admins []string
	// ReadOnly refuses changes to the blacklist and the whitelist.
	ReadOnly bool

	jobs runner
}

// Listen serves one JSON command per connection until ctx ends.
func (s *Server) Listen(ctx context.Context, path string) error {
	if err := openLog(s.DB); err != nil {
		return fmt.Errorf("admin_log schema: %w", err)
	}
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
	defer func() {
		wg.Wait()
		s.jobs.close()
	}()
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
		wg.Go(func() { s.serveConn(conn) })
	}
}

type request struct {
	Op     string       `json:"op"`
	Kind   string       `json:"kind"`
	ID     string       `json:"id"`
	Reason string       `json:"reason"`
	Hash   string       `json:"hash"`
	Filter string       `json:"filter"`
	From   string       `json:"from"`
	To     string       `json:"to"`
	App    string       `json:"app"`
	Note   string       `json:"note"`
	Job    string       `json:"job"`
	Target string       `json:"target"`
	Event  *nostr.Event `json:"event"`
}

// args returns the write arguments that are set.
func (r request) args() args {
	a := args{}
	for key, v := range map[string]string{
		"kind": r.Kind, "id": r.ID, "reason": r.Reason, "hash": r.Hash, "filter": r.Filter,
		"from": r.From, "to": r.To, "app": r.App, "note": r.Note, "job": r.Job,
	} {
		if v = strings.TrimSpace(v); v != "" {
			a[key] = v
		}
	}
	return a
}

type response struct {
	OK      bool       `json:"ok"`
	Error   string     `json:"error,omitempty"`
	Message string     `json:"message,omitempty"`
	Allowed *[]string  `json:"allowed,omitempty"`
	Jobs    *[]JobView `json:"jobs,omitempty"`
	Job     *JobView   `json:"job,omitempty"`
}

func refused(err error) response {
	return response{Error: err.Error()}
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))
	reader := bufio.NewReader(io.LimitReader(conn, 1<<20))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		writeResp(conn, response{Error: "read failed"})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req request
	if err := dec.Decode(&req); err != nil {
		writeResp(conn, response{Error: "invalid json"})
		return
	}
	writeResp(conn, s.handle(conn, req))
}

func (s *Server) handle(conn net.Conn, req request) response {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	switch req.Op {
	case opAllowed:
		keys, err := s.Store.AllowedKeys(ctx)
		if err != nil {
			return refused(err)
		}
		ids := make([]string, len(keys))
		for i, key := range keys {
			ids[i] = key.ID
		}
		return response{OK: true, Allowed: &ids}
	case opJobs:
		if req.Kind != "" && req.Kind != jobSeal && req.Kind != jobEnrich {
			return response{Error: "kind must be seal or enrich"}
		}
		jobs := s.jobs.list(req.Kind, req.Target)
		return response{OK: true, Jobs: &jobs}
	}
	if req.Target != "" {
		return response{Error: req.Op + " does not take target"}
	}

	var e entry
	if req.Event != nil {
		op, a, err := s.verify(req.Event)
		if err != nil {
			slog.Warn("control: signed command refused", "pubkey", req.Event.PubKey, "error", err)
			return refused(err)
		}
		if given := req.args(); (req.Op != "" && req.Op != op) || (len(given) > 0 && !sameArgs(given, a)) {
			return response{Error: "the command does not match the signed event"}
		}
		e = entry{pubkey: req.Event.PubKey, op: op, args: a, eventID: req.Event.ID}
	} else {
		if _, ok := writeOps[req.Op]; !ok {
			return response{Error: "unknown op"}
		}
		uid, err := peerUID(conn)
		if err != nil || (uid != ownUID() && uid != 0) {
			slog.Warn("control: unsigned write refused", "op", req.Op, "uid", uid, "error", err)
			return response{Error: "an unsigned " + req.Op + " is accepted only from the relay's own user"}
		}
		a, err := operatorArgs(req.Op, req.args())
		if err != nil {
			return refused(err)
		}
		if err := checkArgs(req.Op, a); err != nil {
			return refused(err)
		}
		e = entry{op: req.Op, args: a}
	}
	if s.ReadOnly && changesPolicy(e.op) {
		return response{Error: "relay is read-only"}
	}

	e.at = time.Now().Unix()
	row, err := reserve(ctx, s.DB, e)
	if err != nil {
		slog.Warn("control: write refused", "op", e.op, "pubkey", e.pubkey, "error", err)
		return refused(err)
	}
	resp, cmdErr := s.run(ctx, e)
	if err := settle(s.DB, row, cmdErr); err != nil {
		slog.Error("control: admin_log", "error", err)
	}
	slog.Info("control: write", "op", e.op, "args", e.args, "pubkey", e.pubkey, "error", cmdErr)
	if cmdErr != nil {
		return refused(cmdErr)
	}
	resp.OK = true
	return resp
}

// verify checks a signed command and returns what it asks for.
func (s *Server) verify(event *nostr.Event) (string, args, error) {
	op, a, err := signedCommand(s.Relay, event)
	if err != nil {
		return "", nil, err
	}
	if event.ID != event.GetID() {
		return "", nil, errors.New("event id is wrong")
	}
	if ok, err := event.CheckSignature(); !ok || err != nil {
		return "", nil, errors.New("signature is invalid")
	}
	if skew := time.Now().Unix() - int64(event.CreatedAt); skew > signedSkew || skew < -signedSkew {
		return "", nil, errors.New("the signed command is too old; check the device clock")
	}
	if !slices.Contains(s.Admins, strings.ToLower(event.PubKey)) {
		return "", nil, errors.New("this key is not a relay admin")
	}
	return op, a, nil
}

func sameArgs(a, b args) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// operatorArgs keeps the shapes the socket accepted before commands were
// signed: upper-case hex, and for allow and disallow an npub and kind nostr.
func operatorArgs(op string, a args) (args, error) {
	if h, ok := a["hash"]; ok {
		a["hash"] = strings.ToLower(h)
	}
	if op != opAllow && op != opDisallow {
		if kind := a["kind"]; kind == policy.KindNostr || kind == policy.KindCert {
			a["id"] = strings.ToLower(a["id"])
		}
		return a, nil
	}
	if kind, ok := a["kind"]; ok {
		if kind != policy.KindNostr {
			return nil, errors.New(op + " accepts only a nostr key")
		}
		delete(a, "kind")
	}
	if id := a["id"]; strings.HasPrefix(id, "npub1") {
		prefix, v, err := nip19.Decode(id)
		hex, ok := v.(string)
		if err != nil || prefix != "npub" || !ok {
			return nil, errors.New("id is not a valid npub")
		}
		a["id"] = hex
	}
	if id, ok := a["id"]; ok {
		a["id"] = strings.ToLower(id)
	}
	return a, nil
}

func changesPolicy(op string) bool {
	switch op {
	case opBan, opUnban, opUnbanHash, opAllow, opDisallow:
		return true
	}
	return false
}

func (s *Server) run(ctx context.Context, e entry) (response, error) {
	a := e.args
	switch e.op {
	case opBan:
		if s.Ban == nil {
			return response{}, errors.New("ban is not available")
		}
		pub, err := policy.Normalize(a["kind"], a["id"])
		if err != nil {
			return response{}, err
		}
		return response{}, s.Ban(ctx, pub, a["reason"])
	case opUnban:
		return response{}, s.Store.RemovePublisher(ctx, a["kind"], a["id"])
	case opUnbanHash:
		return response{}, s.Store.RemoveHash(ctx, a["hash"])
	case opAllow:
		return response{}, s.Store.Allow(ctx, a["id"])
	case opDisallow:
		return response{}, s.Store.Disallow(ctx, a["id"])
	case opSeal:
		return s.seal(a, e.pubkey)
	case opDeleteBundle:
		return response{}, s.deleteBundle(a)
	case opEnrich:
		return s.enrich(a, e.pubkey)
	case opCancel:
		id, _ := strconv.ParseInt(a["job"], 10, 64)
		return response{}, s.jobs.stop(id)
	}
	return response{}, errors.New("unknown op")
}

func (s *Server) seal(a args, by string) (response, error) {
	if s.Seal == nil {
		return response{}, errors.New("sealing is not available on this relay")
	}
	req := SealRequest{Filter: a["filter"]}
	target := req.Filter
	if raw, ok := a["from"]; ok {
		from, _ := strconv.ParseInt(raw, 10, 64)
		to, err := catalog.LatestSnap(s.Data)
		if err != nil {
			return response{}, err
		}
		if to == 0 || from >= to {
			return response{}, errors.New("from must be older than the current snapshot")
		}
		if from > 0 {
			if _, err := os.Stat(catalog.SnapPath(s.Data, from)); err != nil {
				return response{}, fmt.Errorf("snapshot %d is not on this relay", from)
			}
		}
		req = SealRequest{From: from, To: to, HasFrom: true}
		target = fmt.Sprintf("%d-%d", from, to)
	}
	j, err := s.jobs.start(jobSeal, target, a["note"], by, sealTimeout, func(ctx context.Context) (string, error) {
		return s.Seal(ctx, req)
	})
	if err != nil {
		return response{}, err
	}
	v := j.view()
	return response{Job: &v, Message: fmt.Sprintf("Seal job %d started", v.ID)}, nil
}

func (s *Server) enrich(a args, by string) (response, error) {
	if s.Enrich == nil {
		return response{}, errors.New("enrich is not available on this relay")
	}
	id := a["app"]
	j, err := s.jobs.start(jobEnrich, id, a["note"], by, enrichTimeout, func(ctx context.Context) (string, error) {
		out, err := s.Enrich(ctx, id)
		if err == nil && strings.TrimSpace(out) == "" {
			out = "Nothing to enrich: the app has no listing"
		}
		return out, err
	})
	if err != nil {
		return response{}, err
	}
	v := j.view()
	return response{Job: &v, Message: fmt.Sprintf("Enrich job %d started", v.ID)}, nil
}

// deleteBundle removes one bundle file. A missing file is success.
func (s *Server) deleteBundle(a args) error {
	from, _ := strconv.ParseInt(a["from"], 10, 64)
	to, _ := strconv.ParseInt(a["to"], 10, 64)
	if to < 1 || from >= to {
		return errors.New("from must be older than to")
	}
	err := os.Remove(catalog.BundlePath(s.Data, from, to))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeResp(w io.Writer, resp response) {
	_ = json.NewEncoder(w).Encode(resp)
}
