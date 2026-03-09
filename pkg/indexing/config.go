package indexing

import (
	"errors"
	"fmt"
	"time"
)

// Paths holds filesystem paths for the indexing subsystem.
type Paths struct {
	Store string // path to indexing.db
}

type Config struct {
	// QueueSize is the maximum number of signals that can be buffered in memory.
	// If more signals arrive, they are dropped. Default is 4096.
	QueueSize int `env:"INDEXING_QUEUE_SIZE"`

	// MinTTL is the minimum re-check interval for a frequently-requested app. Default is 1 hour.
	MinTTL time.Duration `env:"INDEXING_MIN_TTL"`

	// MaxTTL is the maximum re-check interval for a rarely-requested app. Default is 7 days.
	MaxTTL time.Duration `env:"INDEXING_MAX_TTL"`
}

func NewConfig() Config {
	return Config{
		QueueSize: defaultQueueSize,
		MinTTL:    minTTL,
		MaxTTL:    maxTTL,
	}
}

func (c Config) Validate() error {
	if c.QueueSize <= 0 {
		return errors.New("queue size must be greater than 0")
	}
	if c.MinTTL <= 0 {
		return errors.New("min TTL must be greater than 0")
	}
	if c.MaxTTL <= 0 {
		return errors.New("max TTL must be greater than 0")
	}
	if c.MinTTL >= c.MaxTTL {
		return errors.New("min TTL must be less than max TTL")
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Indexing:\n"+
		"\tQueue Size: %d\n"+
		"\tMin TTL: %s\n"+
		"\tMax TTL: %s\n",
		c.QueueSize,
		c.MinTTL,
		c.MaxTTL,
	)
}
