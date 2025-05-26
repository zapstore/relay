package main

import (
	"fmt"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

func rejectEvent(c rely.Client, e *nostr.Event) error {
	if (e.Kind != 32267) && (e.Kind != 30063) && (e.Kind != 1063) && (e.Kind != 30267) {
		return fmt.Errorf("blocked: kind %d is not accepted", e.Kind)
	}

	return nil
}

func rejectReq(c rely.Client, f nostr.Filters) error {
	return nil
}
