// The acl package provides access control list functionality for the relay and blossom servers.
// It supports hot-reloadable CSV files for allowed/blocked pubkeys, events, and blobs,
// with a configurable policy for unknown pubkeys (allow, block, or vertex-based reputation filtering).
package acl

import (
	"fmt"
	"slices"

	"github.com/zapstore/relay/pkg/acl/repo"
	"github.com/zapstore/relay/pkg/acl/vertex"
)

// Hardcoded file names within the ACL directory.
const (
	PubkeysAllowedFile      = "pubkeys_allowed.csv"
	PubkeysBlockedFile      = "pubkeys_blocked.csv"
	PlatformUsersBlockedFile = "platform_users_blocked.csv"
)

// RequiredFiles is the list of files that must exist in the ACL directory.
var RequiredFiles = []string{PubkeysAllowedFile, PubkeysBlockedFile, PlatformUsersBlockedFile}

// PubkeyPolicy determines how to handle pubkeys that are not in the allowed or blocked lists.
type PubkeyPolicy string

const (
	// AllowAll allows unknown pubkeys.
	AllowAll PubkeyPolicy = "ALLOW"

	// BlockAll blocks unknown pubkeys.
	BlockAll PubkeyPolicy = "BLOCK"

	// UseVertex uses the Vertex DVM to determine whether to allow or block unknown pubkeys.
	UseVertex PubkeyPolicy = "VERTEX"
)

var PubkeyPolicies = []PubkeyPolicy{AllowAll, BlockAll, UseVertex}

type Config struct {
	// UnknownPubkeyPolicy is the policy to apply to pubkeys not in the allowed or blocked lists.
	// Possible values are "ALLOW", "BLOCK", "VERTEX". Default is "VERTEX".
	UnknownPubkeyPolicy PubkeyPolicy `env:"ACL_UNKNOWN_PUBKEY_POLICY"`

	// Vertex is the configuration for the Vertex DVM, used when PubkeyPolicy is "VERTEX".
	Vertex vertex.Config

	// Repo is the configuration for the platform-agnostic repository verifier.
	Repo repo.Config
}

// NewConfig creates a new Config with default values.
func NewConfig() Config {
	return Config{
		UnknownPubkeyPolicy: UseVertex,
		Vertex:              vertex.NewConfig(),
		Repo:                repo.NewConfig(),
	}
}

// Validate checks that the configuration is valid.
func (c Config) Validate() error {
	if !slices.Contains(PubkeyPolicies, c.UnknownPubkeyPolicy) {
		return fmt.Errorf("unknown pubkey policy: %s. Possible values are: %v", c.UnknownPubkeyPolicy, PubkeyPolicies)
	}

	if c.UnknownPubkeyPolicy == UseVertex {
		if err := c.Vertex.Validate(); err != nil {
			return fmt.Errorf("vertex: %w", err)
		}
	}

	if err := c.Repo.Validate(); err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	return nil
}

// String returns a string representation of the configuration.
func (c Config) String() string {
	s := fmt.Sprintf("ACL:\n"+
		"\tUnknown Pubkey Policy: %s\n",
		c.UnknownPubkeyPolicy)

	if c.UnknownPubkeyPolicy == UseVertex {
		s += c.Vertex.String()
	}

	s += c.Repo.String()
	return s
}
