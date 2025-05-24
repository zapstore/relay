package main

import (
	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

func rejectEvent(c rely.Client, e *nostr.Event) error {
	return nil
}

func rejectReq(c rely.Client, f nostr.Filters) error {
	return nil
}
