package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely/v2"
)

var errCatalogKeySocket = errors.New("catalog key events are accepted only on the local socket")

func catalogKeyOnSocket(catalogKey string) func(rely.Client, *nostr.Event) error {
	return func(_ rely.Client, event *nostr.Event) error {
		if event.PubKey == catalogKey {
			return errCatalogKeySocket
		}
		return nil
	}
}

// ServeNostr serves NIP-01 on one relay.sock connection. One JSON array per line.
// in reads the connection, including any bytes already buffered from it.
// A write is stored only when the event pubkey is the relay's own.
func (r *T) ServeNostr(conn net.Conn, in io.Reader) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	dec := json.NewDecoder(io.LimitReader(in, r.config.MaxMessageBytes))
	w := bufio.NewWriter(conn)
	for {
		var msg []json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				writeNostr(w, "NOTICE", err.Error())
			}
			return
		}
		if len(msg) == 0 {
			writeNostr(w, "NOTICE", "empty message")
			return
		}
		var typ string
		if err := json.Unmarshal(msg[0], &typ); err != nil {
			writeNostr(w, "NOTICE", "invalid message")
			return
		}
		switch typ {
		case "EVENT":
			if len(msg) < 2 {
				writeNostr(w, "NOTICE", "EVENT requires an event")
				return
			}
			var event nostr.Event
			if err := json.Unmarshal(msg[1], &event); err != nil {
				writeNostr(w, "NOTICE", "invalid event")
				return
			}
			ok, reason := r.acceptSocketEvent(&event)
			writeOK(w, event.ID, ok, reason)
		case "REQ":
			if len(msg) < 3 {
				writeNostr(w, "NOTICE", "REQ requires a subscription id and a filter")
				return
			}
			var id string
			if err := json.Unmarshal(msg[1], &id); err != nil || id == "" {
				writeNostr(w, "NOTICE", "invalid subscription id")
				return
			}
			filters := make(nostr.Filters, 0, len(msg)-2)
			for _, raw := range msg[2:] {
				var filter nostr.Filter
				if err := json.Unmarshal(raw, &filter); err != nil {
					writeNostr(w, "CLOSED", id, "invalid filter")
					return
				}
				filters = append(filters, filter)
			}
			if err := r.acceptSocketReq(id, filters); err != nil {
				writeNostr(w, "CLOSED", id, err.Error())
				continue
			}
			events, err := r.query(context.Background(), socketClient{}, id, filters)
			if err != nil {
				writeNostr(w, "CLOSED", id, "query failed")
				continue
			}
			for i := range events {
				writeNostr(w, "EVENT", id, &events[i])
			}
			writeNostr(w, "EOSE", id)
		case "CLOSE":
			return
		default:
			writeNostr(w, "NOTICE", "unsupported message")
			return
		}
	}
}

func (r *T) acceptSocketEvent(event *nostr.Event) (bool, string) {
	if event.PubKey != r.config.Info.Pubkey {
		return false, "this socket accepts only the catalog key"
	}
	client := socketClient{}
	for _, reject := range r.server.Reject.Event {
		err := reject(client, event)
		if errors.Is(err, errCatalogKeySocket) {
			continue
		}
		if err != nil {
			return false, err.Error()
		}
	}
	stored := r.persist(client, event)
	if !stored.ok {
		return false, stored.msg
	}
	if stored.broadcast {
		_ = r.server.Broadcast(event)
	}
	return true, stored.msg
}

func (r *T) acceptSocketReq(id string, filters nostr.Filters) error {
	client := socketClient{}
	for _, reject := range r.server.Reject.Req {
		if err := reject(client, id, filters); err != nil {
			return err
		}
	}
	return nil
}

func writeOK(w *bufio.Writer, id string, ok bool, msg string) {
	writeNostr(w, "OK", id, ok, msg)
}

func writeNostr(w *bufio.Writer, args ...any) {
	raw, err := json.Marshal(args)
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
	_ = w.Flush()
}

// SocketUID is the client id of a relay.sock connection. It skips IP rate limits.
const SocketUID = "relay.sock"

type socketClient struct{}

func (socketClient) UID() string                        { return SocketUID }
func (socketClient) IP() rely.IP                        { return rely.IP{} }
func (socketClient) Pubkeys() []string                  { return nil }
func (socketClient) IsAuthed() bool                     { return false }
func (socketClient) SendAuth()                          {}
func (socketClient) ConnectedAt() time.Time             { return time.Time{} }
func (socketClient) Age() time.Duration                 { return 0 }
func (socketClient) Subscriptions() []rely.Subscription { return nil }
func (socketClient) SendNotice(string)                  {}
func (socketClient) Disconnect()                        {}
func (socketClient) DroppedResponses() int              { return 0 }
func (socketClient) RemainingCapacity() int             { return 1 }
