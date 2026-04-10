package relay

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
	"github.com/zapstore/relay/pkg/events"
)

type Config struct {
	// Hostname is the hostname of the relay, used to validate NIP42 authentication.
	// Default is "".
	Hostname string `env:"RELAY_HOSTNAME"`

	// Port is the port the relay will listen on. Default is "3334".
	Port string `env:"RELAY_PORT"`

	// QueueCapacity is the maximum number of events that can be queued for processing.
	// Default is 1000.
	QueueCapacity int `env:"RELAY_QUEUE_CAPACITY"`

	// MaxMessageBytes is the maximum size of a message that can be sent to the relay.
	// Default is 500_000 (0.5MB).
	MaxMessageBytes int64 `env:"RELAY_MAX_MESSAGE_BYTES"`

	// MaxReqFilters is the maximum number of filters for a single REQ request.
	// Default is 50.
	MaxReqFilters int `env:"RELAY_MAX_REQ_FILTERS"`

	// ResponseLimit is the maximum number of responses that can be buffered and sent
	// to a single client connection before backpressure is applied. Default is 500.
	ResponseLimit int `env:"RELAY_RESPONSE_LIMIT"`

	// AllowedKinds is a list of event kinds that are allowed to be published to the relay.
	// Default is all kinds.
	AllowedKinds []int `env:"RELAY_ALLOWED_EVENT_KINDS"`

	Info Info
}

// NewConfig create a new config with default values.
func NewConfig() Config {
	return Config{
		Port:            "3334",
		QueueCapacity:   1000,
		MaxMessageBytes: 500_000,
		MaxReqFilters:   50,
		ResponseLimit:   500,
		AllowedKinds: []int{
			// app kinds
			events.KindApp,
			events.KindRelease,
			events.KindAsset,
			events.KindStack,
			events.KindAppRelays,

			// social and community kinds
			events.KindProfile,
			events.KindDeletion,
			events.KindForumPost,
			events.KindComment,
			events.KindZap,
			events.KindCommunityCreation,

			// NIP-C1 identity proof kind
			events.KindIdentityProof,
		},
	}
}

// Info stores information about the relay, used in the NIP11 relay information document.
type Info struct {
	Name        string `env:"RELAY_NAME"`
	Pubkey      string `env:"RELAY_PUBKEY"`
	Description string `env:"RELAY_DESCRIPTION"`
	URL         string `env:"RELAY_URL"`
	Contact     string `env:"RELAY_CONTACT"`
	Icon        string `env:"RELAY_ICON"`
	Banner      string `env:"RELAY_BANNER"`
	Software    string `env:"RELAY_SOFTWARE"`
}

func (c Config) Validate() error {
	if c.Hostname == "" {
		return errors.New("hostname is not set")
	}
	if c.Port == "" {
		return errors.New("port is not set")
	}
	if c.QueueCapacity <= 0 {
		return errors.New("queue capacity must be greater than 0")
	}
	if c.MaxMessageBytes <= 0 {
		return errors.New("max message bytes must be greater than 0")
	}
	if c.MaxReqFilters <= 0 {
		return errors.New("max REQ filters must be greater than 0")
	}
	if c.ResponseLimit <= 0 {
		return errors.New("response limit must be greater than 0")
	}
	if len(c.AllowedKinds) == 0 {
		slog.Warn("relay allowed kinds is empty. No events will be accepted.")
	}
	if err := c.Info.Validate(); err != nil {
		// info is not critical, so we log the error and continue
		slog.Error("relay info is invalid or incomplete", "error", err)
	}
	return nil
}

func (i Info) Validate() error {
	if i.Name == "" {
		return errors.New("relay name is not set")
	}
	if i.Pubkey == "" {
		return errors.New("relay pubkey is not set")
	}
	if !nostr.IsValid32ByteHex(i.Pubkey) {
		return errors.New("relay pubkey is not a valid 32 byte hex string")
	}
	if i.Description == "" {
		return errors.New("relay description is not set")
	}
	if i.URL == "" {
		return errors.New("relay URL is not set")
	}
	if i.Contact == "" {
		return errors.New("relay contact is not set")
	}
	if i.Icon == "" {
		return errors.New("relay icon is not set")
	}
	if i.Banner == "" {
		return errors.New("relay banner is not set")
	}
	if i.Software == "" {
		return errors.New("relay software is not set")
	}
	return nil
}

func (i Info) NIP11() nip11.RelayInformationDocument {
	return nip11.RelayInformationDocument{
		Name:        i.Name,
		PubKey:      i.Pubkey,
		Description: i.Description,
		URL:         i.URL,
		Contact:     i.Contact,
		Icon:        i.Icon,
		Banner:      i.Banner,
		Software:    i.Software,
	}
}

func (i Info) String() string {
	return fmt.Sprintf("Info:\n"+
		"\tName: %s\n"+
		"\tPubkey: %s\n"+
		"\tDescription: %s\n"+
		"\tURL: %s\n"+
		"\tContact: %s\n"+
		"\tIcon: %s\n"+
		"\tBanner: %s\n"+
		"\tSoftware: %s\n",
		i.Name, i.Pubkey, i.Description, i.URL, i.Contact, i.Icon, i.Banner, i.Software)
}

func (c Config) String() string {
	return fmt.Sprintf("Relay:\n"+
		"\tHostname: %s\n"+
		"\tPort: %s\n"+
		"\tQueue Capacity: %d\n"+
		"\tMax Message Bytes: %d\n"+
		"\tMax REQ Filters: %d\n"+
		"\tResponse Limit: %d\n"+
		"\tAllowed Kinds: %v\n"+
		c.Info.String(),
		c.Hostname, c.Port, c.QueueCapacity, c.MaxMessageBytes, c.MaxReqFilters, c.ResponseLimit, c.AllowedKinds,
	)
}
