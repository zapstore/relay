package analytics

import (
	"errors"
	"fmt"
	"net"
	"time"
)

type Config struct {
	// Address is the address the analytics API server listens on.
	// Empty disables analytics: no database, no collection, and no API.
	Address string `env:"ANALYTICS_ADDRESS"`

	// FlushInterval is the interval at which the analytics engine flushes data to the database. Default is 5 minutes.
	FlushInterval time.Duration `env:"ANALYTICS_FLUSH_INTERVAL"`

	// FlushTimeout is the maximum time allowed for a flush operation to complete. Default is 10 seconds.
	FlushTimeout time.Duration `env:"ANALYTICS_FLUSH_TIMEOUT"`

	// FlushSize is the maximum number of events that can be flushed to the database in a single transaction. Default is 1000.
	FlushSize int `env:"ANALYTICS_FLUSH_SIZE"`

	// QueueSize is the maximum number of events that can be queued in memory.
	// If more events are queued, they will be dropped. Default is 100_000.
	QueueSize int `env:"ANALYTICS_QUEUE_SIZE"`
}

func NewConfig() Config {
	return Config{
		FlushInterval: 5 * time.Minute,
		FlushTimeout:  10 * time.Second,
		FlushSize:     1000,
		QueueSize:     100_000,
	}
}

// Enabled reports whether ANALYTICS_ADDRESS is set.
func (c Config) Enabled() bool {
	return c.Address != ""
}

func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if _, err := net.ResolveTCPAddr("tcp", c.Address); err != nil {
		return fmt.Errorf("invalid address %q: %w", c.Address, err)
	}
	if c.FlushInterval < time.Second {
		return errors.New("flush interval must be greater than 1s to avoid too many database writes")
	}
	if c.FlushInterval > time.Hour {
		return errors.New("flush interval must be less than 1h to avoid data loss in case of a server crash or restart")
	}
	if c.FlushTimeout <= time.Second {
		return errors.New("flush timeout must be greater than 1s to function reliably")
	}
	if c.FlushTimeout >= c.FlushInterval {
		return errors.New("flush timeout must be less than flush interval to function as intended")
	}
	if c.FlushSize <= 0 {
		return errors.New("flush size must be greater than 0")
	}
	if c.QueueSize <= 0 {
		return errors.New("queue size must be greater than 0")
	}
	return nil
}

func (c Config) String() string {
	if !c.Enabled() {
		return "Analytics:\n\t(unset)\n"
	}
	return fmt.Sprintf("Analytics:\n"+
		"\tAddress: %s\n"+
		"\tFlush Interval: %s\n"+
		"\tFlush Timeout: %s\n"+
		"\tFlush Size: %d\n"+
		"\tQueue Size: %d\n",
		c.Address,
		c.FlushInterval,
		c.FlushTimeout,
		c.FlushSize,
		c.QueueSize)
}
