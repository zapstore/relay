package main

import (
	"context"
	"fmt"
	"log"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

var (
	relay  *rely.Relay
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

	// Rejects
	relay.RejectEvent = append(relay.RejectEvent, rejectEvent)
	relay.RejectReq = append(relay.RejectReq, rejectReq)

	// Logics
	relay.OnEvent = onEvent
	relay.OnReq = onReq

	log.Println("Relay running on port: ", config.RelayPort)

	if err := relay.StartAndServe(ctx, fmt.Sprintf("localhost%s", config.RelayPort)); err != nil {
		log.Fatalf("Can't start the relay: %v", err)
	}
}

func onEvent(rely.Client, *nostr.Event) error {
	return nil
}

func onReq(ctx context.Context, c rely.Client, f nostr.Filters) ([]nostr.Event, error) {
	return []nostr.Event{}, nil
}
