package relay

import (
	_ "embed"
	"fmt"
	"strings"
)

// knownKeyError is a refusal for a certificate whose private key is public.
// The message names where the key is published, which for a debug keystore
// tells the developer what went wrong.
type knownKeyError struct{ msg string }

func (e knownKeyError) Error() string { return e.msg }

// assetKnownKey is the refusal for a developer asset signed by a known key, or nil.
func assetKnownKey(cert string) error {
	note, ok := knownKeys[normalizeCert(cert)]
	if !ok {
		return nil
	}
	return knownKeyError{"the APK is signed with a publicly known private key (" + note + "), so it cannot identify a developer: sign releases with your own key"}
}

// proofKnownKey is the refusal for a kind 30509 naming a known key, or nil.
func proofKnownKey(cert string) error {
	note, ok := knownKeys[normalizeCert(cert)]
	if !ok {
		return nil
	}
	return knownKeyError{"kind 30509: this certificate's private key is publicly known (" + note + "), so a proof for it names no developer"}
}

func normalizeCert(cert string) string { return strings.ToLower(strings.TrimSpace(cert)) }

//go:embed known_keys.txt
var knownKeysFile string

// knownKeys are APK signing certificates whose private key is public, from
// known_keys.txt. A bad line stops the relay at start, and a test reads the
// file, so a bad entry cannot ship.
var knownKeys = mustParseKnownKeys(knownKeysFile)

func mustParseKnownKeys(file string) map[string]string {
	keys, err := parseKnownKeys(file)
	if err != nil {
		panic(err)
	}
	return keys
}

// parseKnownKeys maps each certificate hash to its note.
func parseKnownKeys(file string) (map[string]string, error) {
	keys := map[string]string{}
	for i, line := range strings.Split(file, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hash, note, _ := strings.Cut(line, " ")
		note = strings.TrimSpace(note)
		if len(hash) != 64 || hash != strings.ToLower(hash) || strings.Trim(hash, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("known_keys.txt line %d: %q is not a lowercase SHA-256 hex", i+1, hash)
		}
		if note == "" {
			return nil, fmt.Errorf("known_keys.txt line %d: %s has no source", i+1, hash)
		}
		if _, dup := keys[hash]; dup {
			return nil, fmt.Errorf("known_keys.txt line %d: %s is listed twice", i+1, hash)
		}
		keys[hash] = note
	}
	return keys, nil
}
