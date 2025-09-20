package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

func rejectEvent(c rely.Client, e *nostr.Event) error {
	if e.PubKey == config.RelayPubkey {
		return nil
	}

	if (e.Kind != 32267) && (e.Kind != 30063) && (e.Kind != 1063) &&
		(e.Kind != 30267) && (e.Kind != 3063) && (e.Kind != 4) &&
		(e.Kind != 1) && (e.Kind != 1111) {
		return fmt.Errorf("blocked: you must be whitelisted to publish")
	}

	if e.CreatedAt.Time().After(time.Now()) {
		return errors.New("invalid: event creation date is far from the current time")
	}

	isblackListed, err := db.IsBlacklisted(context.Background(), e.PubKey)
	if err != nil {
		log.Printf("Can't read the blacklist from db: %v\n", err)
		return errors.New("error: reading from database")
	}

	if isblackListed {
		return errors.New("blocked: you are blacklisted")
	}

	// Check if the pubkey has already published anything to our database
	ch, err := db.QueryEvents(context.Background(), nostr.Filter{
		Authors: []string{e.PubKey},
		Limit:   1,
	})
	if err != nil {
		log.Printf("Can't query events from db: %v\n", err)
		return errors.New("error: reading from database")
	}

	hasPublished := false
	for range ch {
		hasPublished = true
		break
	}

	if !hasPublished {
		rank, err := GetWoTRank(e.PubKey)
		if err != nil {
			log.Printf("Can't query WoT Rank from vertex: %v\n", err)
			return errors.New("error: inquiry to vertex")
		}

		if rank < config.WoTThreshold {
			return errors.New("restricted: low WoT rank; contact the Zapstore on the Nostr")
		}
	}

	if e.Kind == 4 {
		for _, t := range e.Tags {
			if len(t) < 2 {
				continue
			}

			if t[0] != "p" {
				continue
			}

			if t[1] != config.RelayPubkey {
				return errors.New("blocked: unsupported kind")
			}
		}
	}

	return nil
}
