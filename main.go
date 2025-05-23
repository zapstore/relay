package main

import (
	"context"
	"log"

	"github.com/pippellia-btc/rely"
)

var (
	relay  *rely.Relay
	config Config
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rely.HandleSignals(cancel)

	relay = rely.NewRelay()
	// rely.Domain = config.RelayURL

	addr := "localhost:3334"
	log.Printf("running relay on %s", addr)

	if err := relay.StartAndServe(ctx, addr); err != nil {
		panic(err)
	}
}
