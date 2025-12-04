package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"path"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

var (
	relay  *rely.Relay
	db     SQLite3Backend
	config Config
)

func main() {
	log.SetPrefix("Relay ")
	log.Printf("Running %s\n", version())

	LoadConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rely.HandleSignals(cancel)

	if !pathExists(config.WorkingDirectory) {
		if err := mkdir(config.WorkingDirectory); err != nil {
			log.Fatalf("can't make a working directory: %v\n", err)
		}
	}

	relay = rely.NewRelay(rely.WithDomain(config.RelayURL))

	db = SQLite3Backend{
		DatabaseURL: path.Join(config.WorkingDirectory, "database.sqlite"),
	}

	if err := db.Init(); err != nil {
		log.Fatalf("can't init database: %v", err)
	}

	relay.RejectEvent = append(relay.RejectEvent, rejectEvent)
	relay.OnEvent = onEvent
	relay.OnReq = onReq

	mux := http.NewServeMux()
	SetupHTTPRoutes(mux)
	mux.Handle("/", relay)

	relay.Start(ctx)

	log.Println("Relay running on port:", config.RelayPort)

	if err := http.ListenAndServe(fmt.Sprintf("localhost%s", config.RelayPort), mux); err != nil {
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

		c := 0
		for e := range ch {
			c++
			evts = append(evts, *e)
		}
	}

	return evts, nil
}
