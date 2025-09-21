package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

var (
	developerKinds = []int{32267, 30063, 1063, 3063}
	userKinds      = []int{30267, 1, 1111, 4}
)

func rejectEvent(c rely.Client, e *nostr.Event) error {
	if e.PubKey == config.RelayPubkey {
		return nil
	}

	if !(slices.Contains(developerKinds, e.Kind) || slices.Contains(userKinds, e.Kind)) {
		return fmt.Errorf("blocked: kind not accepted")
	}

	if e.CreatedAt.Time().After(time.Now()) {
		return errors.New("invalid: event creation date is from the future")
	}

	if slices.Contains(developerKinds, e.Kind) {
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
			Kinds:   []int{32267},
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
			isAboveThreshold, err := IsAboveThreshold(e.PubKey)
			if err != nil {
				log.Printf("Can't query WoT Rank from vertex: %v\n", err)
				return errors.New("error: inquiry to vertex")
			}

			if !isAboveThreshold {
				if err := db.AddToBlacklist(context.Background(), e.PubKey); err != nil {
					log.Printf("Can't insert to db: %v\n", err)
				}
				return errors.New("restricted: low WoT rank; contact the Zapstore on the Nostr")
			}
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
