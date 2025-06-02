package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

func rejectEvent(c rely.Client, e *nostr.Event) error {
	level, err := db.GetWhitelistLevel(context.Background(), e.PubKey)
	if err != nil {
		log.Printf("Can't read the whitelist from db: %v\n", err)
		return errors.New("internal: reading from database")
	}

	if level == 0 && e.PubKey != config.RelayPubkey {
		if (e.Kind != 4) && (e.Kind != 30267) {
			return fmt.Errorf("blocked: kind %d is not accepted from public", e.Kind)
		}
	}

	// Developer
	if level == 1 && e.PubKey != config.RelayPubkey {
		if (e.Kind != 32267) && (e.Kind != 30063) && (e.Kind != 1063) &&
			(e.Kind != 30267) && (e.Kind != 3063) && (e.Kind != 4) {
			return fmt.Errorf("blocked: kind %d is not accepted from devs", e.Kind)
		}
	}

	// User
	if level == 2 && e.PubKey != config.RelayPubkey {
		if (e.Kind != 1) && (e.Kind != 1111) && (e.Kind != 4) && (e.Kind != 30267) {
			return fmt.Errorf("blocked: kind %d is not accepted from users", e.Kind)
		}
	}

	// User + Dev
	if level == 3 && e.PubKey != config.RelayPubkey {
		if (e.Kind != 32267) && (e.Kind != 30063) && (e.Kind != 1063) &&
			(e.Kind != 30267) && (e.Kind != 3063) && (e.Kind != 4) &&
			(e.Kind != 1) && (e.Kind != 1111) {
			return fmt.Errorf("blocked: kind %d is not accepted from devs and users", e.Kind)
		}
	}

	if e.Kind == 4 && e.PubKey != config.RelayPubkey {
		for _, t := range e.Tags {
			if len(t) < 2 {
				continue
			}

			if t[0] != "p" {
				continue
			}

			if t[1] != config.RelayPubkey {
				return errors.New("blocked: only DMs to relay pubkey is accepted")
			}
		}
	}

	return nil
}
