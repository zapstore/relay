package main

import (
	"context"
	"fmt"
	"log"
	"path"

	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

var (
	relay  *rely.Relay
	db     sqlite3.SQLite3Backend
	config Config
)

func main() {
	log.SetPrefix("Relay ")
	log.Printf("Running %s\n", StringVersion())

	LoadConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rely.HandleSignals(cancel)

	relay = rely.NewRelay(rely.WithDomain(config.RelayURL))

	db = sqlite3.SQLite3Backend{
		DatabaseURL: path.Join(config.WorkingDirectory, "database"),
	}

	if err := db.Init(); err != nil {
		log.Fatalf("can't init database: %v", err)
	}

	// Rejects
	relay.RejectEvent = append(relay.RejectEvent, rejectEvent)
	relay.RejectReq = append(relay.RejectReq, rejectReq)

	// Logics
	relay.OnEvent = onEvent
	relay.OnReq = onReq

	log.Println("Relay running on port: ", config.RelayPort)

	if err := relay.StartAndServe(ctx, fmt.Sprintf("localhost%s", config.RelayPort)); err != nil {
		db.Close()
		log.Fatalf("Can't start the relay: %v", err)
	}
}

func onEvent(_ rely.Client, e *nostr.Event) error {
	if nostr.IsEphemeralKind(e.Kind) {
		return nil
	}

	if nostr.IsRegularKind(e.Kind) {
		return db.SaveEvent(context.Background(), e)
	}

	if nostr.IsReplaceableKind(e.Kind) || nostr.IsAddressableKind(e.Kind) {
		return db.ReplaceEvent(context.Background(), e)
	}

	return nil
}

func onReq(ctx context.Context, c rely.Client, filters nostr.Filters) ([]nostr.Event, error) {
	evts := make([]nostr.Event, 0)

	for _, f := range filters {
		ch, err := db.QueryEvents(context.Background(), f)
		if err != nil {
			return nil, err
		}

		for e := range ch {
			evts = append(evts, *e)
		}
	}

	return evts, nil
}
