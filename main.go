package main

import (
	"context"
	"fmt"
	"log"

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

	relay = rely.NewRelay()
	// rely.Domain = config.RelayURL

	log.Println("Relay running on port: ", config.RelayPort)

	if err := relay.StartAndServe(ctx, fmt.Sprintf("localhost%s", config.RelayPort)); err != nil {
		log.Fatal("Can't start the relay: %v", err)
	}
}
