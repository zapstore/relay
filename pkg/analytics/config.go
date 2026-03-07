package analytics

import (
	"errors"
	"fmt"
	"time"

	"github.com/zapstore/relay/pkg/analytics/geo"
)

type Config struct {
	// FlushInterval is the interval at which the analytics engine flushes data to the database. Default is 5 minutes.
	FlushInterval time.Duration `env:"ANALYTICS_FLUSH_INTERVAL"`

	// FlushTimeout is the maximum time allowed for a flush operation to complete. Default is 10 seconds.
	FlushTimeout time.Duration `env:"ANALYTICS_FLUSH_TIMEOUT"`

	// FlushSize is the maximum number of events that can be flushed to the database in a single transaction. Default is 1000.
	FlushSize int `env:"ANALYTICS_FLUSH_SIZE"`

	// QueueSize is the maximum number of events that can be queued in memory.
	// If more events are queued, they will be dropped. Default is 100_000.
	QueueSize int `env:"ANALYTICS_QUEUE_SIZE"`

	// GeoEnabled is a flag indicating whether geo-location (country code) should be collected and stored. Default is true.
	GeoEnabled bool `env:"ANALYTICS_GEO_ENABLED"`

	// GeoRefreshInterval is the interval at which the geo-location database is refreshed. Default is 24 hours.
	GeoRefreshInterval time.Duration `env:"ANALYTICS_GEO_REFRESH_INTERVAL"`

	Geo geo.Config
}

func NewConfig() Config {
	return Config{
		FlushInterval:      5 * time.Minute,
		FlushTimeout:       10 * time.Second,
		FlushSize:          1000,
		QueueSize:          100_000,
		GeoEnabled:         true,
		GeoRefreshInterval: 24 * time.Hour,
		Geo:                geo.NewConfig(),
	}
}

func (c Config) Validate() error {
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
	if c.GeoEnabled {
		if c.GeoRefreshInterval < time.Hour {
			return errors.New("geo refresh interval must be at least 1 hour to avoid rate-limits")
		}
		if err := c.Geo.Validate(); err != nil {
			return fmt.Errorf("geo config: %w", err)
		}
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Analytics:\n"+
		"\tFlush Interval: %s\n"+
		"\tFlush Timeout: %s\n"+
		"\tFlush Size: %d\n"+
		"\tQueue Size: %d\n"+
		"\tGeo Enabled: %t\n"+
		"\tGeo Refresh Interval: %s\n"+
		"%s",
		c.FlushInterval,
		c.FlushTimeout,
		c.FlushSize,
		c.QueueSize,
		c.GeoEnabled,
		c.GeoRefreshInterval,
		c.Geo.String())
}
