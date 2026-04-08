// The vertex package exposes a configurable [Filter] struct that allows or rejects a pubkey
// based on its reputation.
package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/nbd-wtf/go-nostr"
)

const Endpoint = "https://relay.vertexlab.io/api/v1/dvms"

const (
	KindVerifyReputation = 5312
	KindRecommendFollows = 5313
	KindRankProfiles     = 5314
	KindSearchProfiles   = 5315
	KindDVMError         = 7000
)

// Filter is responsible for allowing based on the reputation of a pubkey.
// It stores ranks in a LRU cache with size and time to live specified in the config.
type Filter struct {
	http   *http.Client
	cache  *expirable.LRU[string, float64]
	config Config
}

// NewFilter creates a new Filter with the given config.
func NewFilter(c Config) Filter {
	return Filter{
		http:   &http.Client{Timeout: c.Timeout},
		cache:  expirable.NewLRU[string, float64](c.CacheSize, nil, c.CacheExpiration),
		config: c,
	}
}

type Response struct {
	Pubkey    string  `json:"pubkey"`
	Rank      float64 `json:"rank"`
	Follows   int     `json:"follows"`
	Followers int     `json:"followers"`

	// present only if the profile leaked its key
	Leak *Leak `json:"leak,omitempty"`
}

type Leak struct {
	Status     string `json:"status"`
	Proof      string `json:"proof,omitempty"`
	DetectedAt int64  `json:"detected_at,omitempty"`
}

// Allow returns true if the pubkey is considered trustworthy, otherwise false.
// It returns an error if the request to Vertex fails.
func (f Filter) Allow(ctx context.Context, pubkey string) (bool, error) {
	if f.config.Algorithm.Threshold <= 0 {
		return true, nil
	}

	if rank, ok := f.cache.Get(pubkey); ok {
		return rank >= f.config.Algorithm.Threshold, nil
	}

	payload := nostr.Event{
		Kind:      KindVerifyReputation,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"param", "target", pubkey},
			{"param", "limit", "0"}, // don't need top followers
			{"param", "sort", string(f.config.Algorithm.Sort)},
			{"param", "source", f.config.Algorithm.Source},
		},
	}

	response, err := f.DVM(ctx, payload)
	if err != nil {
		return false, fmt.Errorf("vertex.Filter.Allow: %w", err)
	}

	var ranks []Response
	if err := json.Unmarshal([]byte(response.Content), &ranks); err != nil {
		return false, fmt.Errorf("vertex.Filter: failed to unmarshal the response event content: %w", err)
	}

	if len(ranks) == 0 {
		return false, fmt.Errorf("vertex.Filter: received an empty response")
	}

	target := ranks[0]
	if target.Pubkey != pubkey {
		return false, fmt.Errorf("vertex.Filter: received a response for a different pubkey: expected %s, got %s", pubkey, target.Pubkey)
	}
	if target.Leak != nil {
		// a leaked key is by definition not trustworthy. We cache it to avoid repeated lookups.
		f.cache.Add(target.Pubkey, -1)
		return false, nil
	}

	f.cache.Add(target.Pubkey, target.Rank)
	return target.Rank >= f.config.Algorithm.Threshold, nil
}

// DVM makes an API call to the Vertex API /dvms endpoint, writing the specified nostr event into the body.
// It returns the DVM response or an error if any. Kind 7000 are considered errors.
func (f Filter) DVM(ctx context.Context, payload nostr.Event) (nostr.Event, error) {
	if err := payload.Sign(f.config.SecretKey); err != nil {
		return nostr.Event{}, fmt.Errorf("failed to sign the request: %w", err)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("failed to marshal the API payload: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, Endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("failed to create the API request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := f.http.Do(request)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("failed to send the API request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		return nostr.Event{}, fmt.Errorf("unexpected status code: %d, body: %s", response.StatusCode, string(body))
	}

	var result nostr.Event
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nostr.Event{}, fmt.Errorf("failed to decode the API response: %w", err)
	}

	switch result.Kind {
	case KindDVMError:
		msg := "unknown error"
		status := result.Tags.Find("status")
		if len(status) > 2 {
			msg = status[2]
		}
		return nostr.Event{}, fmt.Errorf("received a DVM error: %s", msg)

	case payload.Kind + 1000:
		return result, nil

	default:
		return nostr.Event{}, fmt.Errorf("received an unknown kind: expected %d, got %d", payload.Kind+1000, result.Kind)
	}
}
