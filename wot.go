package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

type VertexResponse struct {
	Pubkey string  `json:"pubkey"`
	Rank   float64 `json:"rank"`
}

func IsAboveThreshold(pubkey string) (bool, error) {
	relay, err := nostr.RelayConnect(context.Background(), "wss://relay.vertexlab.io")
	if err != nil {
		return false, err
	}

	verifyReputation := &nostr.Event{
		Kind:      5312,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"param", "target", pubkey},
			{"param", "limit", "7"},
		},
	}

	err = verifyReputation.Sign(config.PrivateKey)
	if err != nil {
		return false, err
	}

	err = relay.Publish(context.Background(), *verifyReputation)
	if err != nil {
		return false, err
	}

	filter := nostr.Filter{
		Kinds: []int{6312, 7000},
		Tags: nostr.TagMap{
			"e": {verifyReputation.ID},
		},
	}

	responses, err := relay.QueryEvents(context.Background(), filter)
	if err != nil {
		return false, err
	}

	response := new(nostr.Event)
	select {
	case response = <-responses:
	case <-time.After(10 * time.Second):
		return false, errors.New("timeout waiting for vertex response")
	}

	if response.Kind == 7000 {
		err := ""
		for _, t := range response.Tags {
			if len(t) >= 3 && t[1] == "error" {
				err = t[2]
				break
			}
		}
		return false, errors.New(err)
	}

	rank := new(VertexResponse)
	if err := json.Unmarshal([]byte(response.Content), rank); err != nil {
		return false, err
	}

	if rank.Pubkey != pubkey {
		return false, errors.New("internal error: invalid response from vertex")
	}

	return rank.Rank > config.WoTThreshold, nil
}
