package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/relay"
)

const (
	siteFile = "launch.json"
	// oldKey is the catalog key before rotation.
	oldKey = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"
)

// site is what the operator told launch about this server.
type site struct {
	RelayUnit   string `json:"relay_unit"`
	IndexerUnit string `json:"indexer_unit"`
	ProxyUnit   string `json:"proxy_unit"`
	RelayEnv    string `json:"relay_env"`
	IndexerEnv  string `json:"indexer_env"`
	RelayBinary string `json:"relay_binary"`
	// RelaySocket is relay.sock. The relay unit sets its path, not the env file.
	RelaySocket string `json:"relay_socket"`
	BackupRoot  string `json:"backup_root"`
}

// paths are read from the env files.
type paths struct {
	RelayData   string // DATA_DIR
	IndexerData string // STORE_DIR
	RelayAddr   string // RELAY_ADDRESS
	RelaySocket string
}

func (p paths) relayDB() string   { return filepath.Join(p.RelayData, "relay.db") }
func (p paths) indexerDB() string { return filepath.Join(p.IndexerData, "indexer.db") }

var siteDefaults = site{
	RelayUnit:   "relay",
	IndexerUnit: "indexer",
	ProxyUnit:   "caddy",
	RelayEnv:    "/opt/zapstore/env/relay.env",
	IndexerEnv:  "/opt/zapstore/env/indexer.env",
	RelayBinary: "/usr/local/bin/relay",
	RelaySocket: "/run/zapstore-relay/relay.sock",
	BackupRoot:  "/var/backups/zapstore-launch",
}

// loadSite asks for every answer, offering the saved one, and checks them.
func loadSite(ctx context.Context, t term) (site, error) {
	s := siteDefaults
	raw, err := os.ReadFile(siteFile)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s); err != nil {
			return site{}, fmt.Errorf("read %s: %w", siteFile, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return site{}, err
	}
	for {
		fields := []struct {
			prompt string
			value  *string
		}{
			{"Relay systemd unit", &s.RelayUnit},
			{"Indexer systemd unit", &s.IndexerUnit},
			{"Reverse proxy systemd unit", &s.ProxyUnit},
			{"Relay env file", &s.RelayEnv},
			{"Indexer env file", &s.IndexerEnv},
			{"Installed relay binary", &s.RelayBinary},
			{"Relay socket", &s.RelaySocket},
			{"Backup directory", &s.BackupRoot},
		}
		for _, f := range fields {
			answer, err := t.ask(f.prompt, *f.value)
			if err != nil {
				return site{}, err
			}
			*f.value = answer
		}
		p, problems := s.check(ctx)
		if len(problems) == 0 {
			raw, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				return site{}, err
			}
			if err := os.WriteFile(siteFile, append(raw, '\n'), 0o600); err != nil {
				return site{}, err
			}
			t.say("Relay data:   %s", p.RelayData)
			t.say("Indexer data: %s", p.IndexerData)
			t.say("Relay:        %s", p.RelayAddr)
			return s, nil
		}
		for _, problem := range problems {
			t.say("  ! %s", problem)
		}
		if err := t.confirm("Answer again?"); err != nil {
			return site{}, err
		}
	}
}

// check returns the paths, or every problem it found.
func (s site) check(ctx context.Context) (paths, []string) {
	var problems []string
	for _, unit := range []string{s.RelayUnit, s.IndexerUnit, s.ProxyUnit} {
		if out, err := exec.CommandContext(ctx, "systemctl", "cat", unit).CombinedOutput(); err != nil {
			problems = append(problems, fmt.Sprintf("unit %s: %s", unit, firstLine(out, err)))
		}
	}
	p, err := s.paths()
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		for _, file := range []string{p.relayDB(), p.indexerDB()} {
			if _, err := os.Stat(file); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if out, err := exec.CommandContext(ctx, s.RelayBinary, "version").CombinedOutput(); err != nil {
		problems = append(problems, fmt.Sprintf("%s version: %s", s.RelayBinary, firstLine(out, err)))
	}
	if err := os.MkdirAll(s.BackupRoot, 0o700); err != nil {
		problems = append(problems, err.Error())
	}
	return p, problems
}

func (s site) paths() (paths, error) {
	relayEnv, err := readEnv(s.RelayEnv)
	if err != nil {
		return paths{}, err
	}
	indexerEnv, err := readEnv(s.IndexerEnv)
	if err != nil {
		return paths{}, err
	}
	data := relayEnv["DATA_DIR"]
	if data == "" {
		return paths{}, fmt.Errorf("%s: DATA_DIR is not set", s.RelayEnv)
	}
	store := indexerEnv["STORE_DIR"]
	if store == "" {
		return paths{}, fmt.Errorf("%s: STORE_DIR is not set", s.IndexerEnv)
	}
	addr := relayEnv["RELAY_ADDRESS"]
	if addr == "" {
		addr = "localhost:3334"
	}
	return paths{RelayData: data, IndexerData: store, RelayAddr: addr, RelaySocket: s.RelaySocket}, nil
}

func readEnv(path string) (map[string]string, error) {
	env, err := godotenv.Read(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return env, nil
}

// signerFor opens SIGN_WITH from an env file. ctx must outlive the signer.
func signerFor(ctx context.Context, envFile string) (catalog.Signer, error) {
	env, err := readEnv(envFile)
	if err != nil {
		return catalog.Signer{}, err
	}
	signer, err := catalog.OpenSigner(ctx, env["SIGN_WITH"])
	if err != nil {
		return catalog.Signer{}, fmt.Errorf("%s SIGN_WITH: %w", envFile, err)
	}
	return signer, nil
}

// relayEnvWant is what the relay env file must say.
type relayEnvWant struct {
	key       string // RELAY_PUBKEY; empty means any key but oldKey
	intervals bool   // CATALOG_BUNDLE_INTERVAL and BLOSSOM_PRUNE_INTERVAL on, else both 0
}

var intervalNames = []string{"CATALOG_BUNDLE_INTERVAL", "BLOSSOM_PRUNE_INTERVAL"}

// checkRelayEnv returns the relay's key and its signer, or what is wrong.
func checkRelayEnv(ctx context.Context, envFile string, want relayEnvWant) (string, catalog.Signer, error) {
	env, err := readEnv(envFile)
	if err != nil {
		return "", catalog.Signer{}, err
	}
	key, err := relay.PubkeyHex(env["RELAY_PUBKEY"])
	if err != nil || len(key) != 64 {
		return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is not a public key")
	}
	switch {
	case want.key != "" && key != want.key:
		return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is %s, want %s", key, want.key)
	case want.key == "" && key == oldKey:
		return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is still 78ce…")
	}
	for _, name := range intervalNames {
		on, err := intervalOn(env[name])
		if err != nil {
			return "", catalog.Signer{}, fmt.Errorf("%s: %w", name, err)
		}
		if on != want.intervals {
			if want.intervals {
				return "", catalog.Signer{}, fmt.Errorf("%s is 0; set it or remove the line", name)
			}
			return "", catalog.Signer{}, fmt.Errorf("%s must be 0", name)
		}
	}
	signer, err := signerFor(ctx, envFile)
	if err != nil {
		return "", catalog.Signer{}, err
	}
	if signer.PubKey != key {
		return "", catalog.Signer{}, fmt.Errorf("SIGN_WITH signs as %s, not RELAY_PUBKEY", signer.PubKey)
	}
	return key, signer, nil
}

// intervalOn reports whether a duration setting is on. Unset is the relay default, which is on.
func intervalOn(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return true, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return false, fmt.Errorf("%q is not a duration", value)
	}
	return d > 0, nil
}

// editRelayEnv waits for the operator to edit the env file, then checks it until it is right.
func editRelayEnv(ctx context.Context, t term, envFile string, want relayEnvWant, lines ...string) (string, catalog.Signer, error) {
	for {
		if err := t.wait(append([]string{"Edit " + envFile + ":"}, lines...)...); err != nil {
			return "", catalog.Signer{}, err
		}
		key, signer, err := checkRelayEnv(ctx, envFile, want)
		if err == nil {
			return key, signer, nil
		}
		t.say("  ! %v", err)
	}
}

func firstLine(out []byte, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if line == "" {
		return err.Error()
	}
	return line
}
