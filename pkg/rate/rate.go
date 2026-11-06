// The rate package is responsible for rate limiting the relay and blossom servers.
// It exposes a [NewLimiter] function to create a new rate limiter with the given config.
package rate

import (
	"math"
	"time"

	"github.com/pippellia-btc/rate"
)

// Limiter is a wrapper around the [rate.Limiter] that adds a [Config] to the limiter.
type Limiter struct {
	*rate.Limiter[string]
	config Config
}

// NewLimiter creates a new rate limiter with a [rate.FlatRefiller] from the given config.
func NewLimiter(c Config) Limiter {
	refiller := rate.FlatRefiller[string]{
		InitialTokens:     float64(c.InitialTokens),
		MaxTokens:         float64(c.MaxTokens),
		TokensPerInterval: float64(c.TokensPerInterval),
		Interval:          c.Interval,
	}

	return Limiter{
		Limiter: rate.NewLimiter(refiller),
		config:  c,
	}
}

// RetryAfter is an upper bound on the wait until the entity can afford cost.
// Tokens arrive TokensPerInterval at a time, once per Interval.
func (l Limiter) RetryAfter(entity string, cost float64) time.Duration {
	need := cost - l.Balance(entity)
	if need <= 0 {
		return 0
	}
	if l.config.TokensPerInterval <= 0 {
		return l.config.Interval
	}
	intervals := math.Ceil(need / float64(l.config.TokensPerInterval))
	return time.Duration(intervals) * l.config.Interval
}

func (l Limiter) InitialTokens() float64     { return float64(l.config.InitialTokens) }
func (l Limiter) MaxTokens() float64         { return float64(l.config.MaxTokens) }
func (l Limiter) TokensPerInterval() float64 { return float64(l.config.TokensPerInterval) }
func (l Limiter) Interval() time.Duration    { return l.config.Interval }
