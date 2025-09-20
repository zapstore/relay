package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nbd-wtf/go-nostr"
)

type VertexResponse struct {
	Pubkey string `json:"pubkey"`
	Rank   float64 `json:"rank"`
}

func GetWoTRank(pubkey string) (float64, error) {
	relay, err := nostr.RelayConnect(context.Background(), "wss://relay.vertexlab.io")
	if err != nil {
		return 0, err
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
		return 0, err
	}

	err = relay.Publish(context.Background(), *verifyReputation)
	if err != nil {
		return 0, err
	}

	filter := nostr.Filter{
		Kinds: []int{6312, 7000},
		Tags: nostr.TagMap{
			"e": {verifyReputation.ID},
		},
	}

	responses, err := relay.QueryEvents(context.Background(), filter)
	if err != nil {
		return 0, err
	}

	response := <-responses

	rank := new(VertexResponse)
	if err := json.Unmarshal([]byte(response.Content), rank); err != nil {
		return 0, err
	}

	if rank.Pubkey != pubkey {
		return 0, errors.New("internal error: invalid response from vertex")
	}

	return rank.Rank, nil
}
