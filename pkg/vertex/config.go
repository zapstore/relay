package vertex

import (
	"errors"
	"fmt"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Config is the Vertex web-of-trust check. An empty SecretKey disables it,
// and the publisher gate then admits the catalog key, whitelisted keys, and
// keys that have already published a kind 32267.
type Config struct {
	// The algorithm to use to decide whether to allow a pubkey.
	Algorithm Algorithm

	// The secret key to use for signing requests to the Vertex DVM.
	// Empty disables the check.
	SecretKey string `env:"VERTEX_SECRET_KEY"`

	// RequestTimeout is the maximum time to wait for a response from Vertex. Default is 10 seconds.
	RequestTimeout time.Duration `env:"VERTEX_REQUEST_TIMEOUT"`

	// CreditsPollInterval is the interval at which to poll for credits. Default is 1 hour.
	CreditsPollInterval time.Duration `env:"VERTEX_CREDITS_POLL_INTERVAL"`

	// CreditsLogThreshold is the threshold below which a credit warning will be logged. Default is 500.
	CreditsLogThreshold int64 `env:"VERTEX_CREDITS_LOG_THRESHOLD"`

	// CacheExpiration time for ranks in the cache. Default is 24 hours.
	CacheExpiration time.Duration `env:"VERTEX_CACHE_EXPIRATION"`

	// CacheSize is the maximum number of entries in the cache. Default is 100_000.
	CacheSize int `env:"VERTEX_CACHE_SIZE"`
}

type Algorithm struct {
	// The sorting algorithm to use. Default is "globalPagerank".
	Sort Sort `env:"VERTEX_SORT"`

	// The source of pubkey, used when the Sort is SortPersonalized. Default is "".
	Source string `env:"VERTEX_SOURCE"`

	// The threshold above which an unknown pubkey is allowed.
	// Default is 0.000005.
	Threshold float64 `env:"VERTEX_THRESHOLD"`
}

// Sort specifies the sorting algorithm to use for pubkey ranking.
type Sort string

const (
	SortGlobal       Sort = "globalPagerank"
	SortPersonalized Sort = "personalizedPagerank"
	SortFollowers    Sort = "followerCount"
)

func NewConfig() Config {
	return Config{
		Algorithm:           Algorithm{Sort: SortGlobal, Threshold: 0.000005},
		RequestTimeout:      10 * time.Second,
		CreditsPollInterval: 1 * time.Hour,
		CreditsLogThreshold: 500,
		CacheExpiration:     24 * time.Hour,
		CacheSize:           100_000,
	}
}

// Enabled reports whether a secret key is set.
func (c Config) Enabled() bool {
	return c.SecretKey != ""
}

func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if err := c.Algorithm.Validate(); err != nil {
		return err
	}
	if !nostr.IsValid32ByteHex(c.SecretKey) {
		return errors.New("secret key is not a valid 32 byte hex string")
	}

	if c.RequestTimeout < time.Second {
		return errors.New("timeout must be greater than 1 second to function reliably")
	}
	if c.CreditsPollInterval < time.Minute {
		return errors.New("credits poll interval must be greater than 1 minute, otherwise it's too frequent")
	}
	if c.CreditsLogThreshold < 0 {
		return errors.New("credits log threshold must be greater than 0")
	}

	if c.CacheExpiration < time.Minute {
		return errors.New("cache expiration must be greater than 1 minute, or it's ineffective")
	}
	if c.CacheSize <= 0 {
		return errors.New("cache size must be greater than 0")
	}
	return nil
}

func (a Algorithm) Validate() error {
	switch a.Sort {
	case SortGlobal:
		if a.Threshold < 0.0 || a.Threshold > 1.0 {
			return fmt.Errorf("threshold must be between 0.0 and 1.0 for %s", a.Sort)
		}
		return nil

	case SortPersonalized:
		if a.Source == "" {
			return fmt.Errorf("source is empty or not set for %s", a.Sort)
		}
		if !nostr.IsValid32ByteHex(a.Source) {
			return fmt.Errorf("source is not a valid 32 byte hex string for %s", a.Sort)
		}

		if a.Threshold < 0.0 || a.Threshold > 1.0 {
			return fmt.Errorf("threshold must be between 0.0 and 1.0 for %s", a.Sort)
		}
		return nil

	case SortFollowers:
		if a.Threshold < 0 {
			return fmt.Errorf("threshold must be greater than 0 for %s", a.Sort)
		}
		return nil

	default:
		return fmt.Errorf("invalid sort: %s", a.Sort)
	}
}

func (c Config) String() string {
	sk := "[not set]"
	if c.SecretKey != "" {
		sk = c.SecretKey[:4] + "___REDACTED___" + c.SecretKey[len(c.SecretKey)-4:]
	}

	return fmt.Sprintf("Vertex:\n"+
		"\tSecret Key: %s\n"+
		"\tRequest Timeout: %v\n"+
		"\tCredits Poll Interval: %v\n"+
		"\tCredits Log Threshold: %d\n"+
		"\tCache Expiration: %v\n"+
		"\tCache Size: %d\n"+
		"\tAlgorithm:\n"+
		"\t\tSource: %s\n"+
		"\t\tSort: %s\n"+
		"\t\tThreshold: %f\n",
		sk,
		c.RequestTimeout,
		c.CreditsPollInterval,
		c.CreditsLogThreshold,
		c.CacheExpiration,
		c.CacheSize,
		c.Algorithm.Source,
		c.Algorithm.Sort,
		c.Algorithm.Threshold,
	)
}
