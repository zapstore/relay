package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	BackupRoot  string `json:"backup_root"`
	// OldUnit and OldData are the v1 relay from a deploy-v2 backup.
	// Restore starts OldUnit again. They are not asked for.
	OldUnit string `json:"old_unit,omitempty"`
	OldData string `json:"old_data,omitempty"`
	// These are used when the matching env file cannot be read.
	RelayData   string `json:"relay_data,omitempty"`
	RelayAddr   string `json:"relay_address,omitempty"`
	SocketPath  string `json:"socket_path,omitempty"`
	IndexerData string `json:"indexer_data,omitempty"`
}

// paths are read from the env files.
type paths struct {
	RelayData   string // DATA_DIR
	IndexerData string // DATA_DIR
	RelayAddr   string // RELAY_ADDRESS
	RelaySocket string
}

// siteFromEnv applies LAUNCH_* overrides. A set variable wins over launch.json.
func siteFromEnv(s site) site {
	set := func(key string, dst *string) {
		v, ok := os.LookupEnv(key)
		v = strings.TrimSpace(v)
		if ok && v != "" {
			*dst = v
		}
	}
	set("LAUNCH_RELAY_UNIT", &s.RelayUnit)
	set("LAUNCH_INDEXER_UNIT", &s.IndexerUnit)
	set("LAUNCH_PROXY_UNIT", &s.ProxyUnit)
	set("LAUNCH_RELAY_ENV", &s.RelayEnv)
	set("LAUNCH_INDEXER_ENV", &s.IndexerEnv)
	set("LAUNCH_RELAY_BINARY", &s.RelayBinary)
	set("LAUNCH_BACKUP_ROOT", &s.BackupRoot)
	return s
}

func (p paths) relayDB() string   { return filepath.Join(p.RelayData, "relay.db") }
func (p paths) indexerDB() string { return filepath.Join(p.IndexerData, "indexer.db") }

var siteDefaults = site{
	RelayUnit:   "relay",
	IndexerUnit: "indexer",
	ProxyUnit:   "caddy",
	RelayEnv:    "/opt/zapstore/env/relay.env",
	IndexerEnv:  "/opt/zapstore/env/indexer.env",
	RelayBinary: "/opt/zapstore/bin/relay",
	BackupRoot:  "/var/backups/zapstore-launch",
}

// loadSite asks for every answer, offering the saved one, and checks them.
func loadSite(ctx context.Context, t term) (site, error) {
	if err := requireFTS5(); err != nil {
		return site{}, err
	}
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
	s = siteFromEnv(s)
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
			{"Backup directory", &s.BackupRoot},
		}
		for _, f := range fields {
			answer, err := t.ask(ctx, f.prompt, *f.value)
			if err != nil {
				return site{}, err
			}
			*f.value = answer
		}
		if err := s.promptUnread(ctx, t); err != nil {
			return site{}, err
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
			t.say("Relay socket: %s", p.RelaySocket)
			if info, err := os.Stat(p.RelaySocket); err != nil || info.Mode()&os.ModeSocket == 0 {
				t.say("The running relay has no socket there. Relay v2 creates it when it starts.")
			}
			return s, nil
		}
		for _, problem := range problems {
			t.say("  ! %s", problem)
		}
		if err := t.confirm(ctx, "Answer again?"); err != nil {
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
	p, err := s.paths(ctx)
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
	if p.RelayData != "" && within(s.BackupRoot, p.RelayData) {
		problems = append(problems, "the backup directory is inside the relay data")
	}
	if p.IndexerData != "" && within(s.BackupRoot, p.IndexerData) {
		problems = append(problems, "the backup directory is inside the indexer data")
	}
	if err := os.MkdirAll(s.BackupRoot, 0o700); err != nil {
		problems = append(problems, err.Error())
	}
	return p, problems
}

// promptUnread asks for the settings an unreadable env file would have supplied.
// Secrets are not asked for and not stored. A readable file is left to paths.
func (s *site) promptUnread(ctx context.Context, t term) error {
	if _, err := readEnv(s.RelayEnv); err != nil {
		t.say("Cannot read %s (%v).", s.RelayEnv, err)
		data, err := t.ask(ctx, "Relay DATA_DIR", s.RelayData)
		if err != nil {
			return err
		}
		addr, err := t.ask(ctx, "RELAY_ADDRESS", firstNonEmpty(s.RelayAddr, "localhost:3334"))
		if err != nil {
			return err
		}
		sock, err := t.ask(ctx, "RELAY_SOCKET_PATH (empty uses the unit or relay.sock in DATA_DIR)", s.SocketPath)
		if err != nil {
			return err
		}
		s.RelayData, s.RelayAddr, s.SocketPath = data, addr, sock
	}
	if _, err := readEnv(s.IndexerEnv); err != nil {
		t.say("Cannot read %s (%v).", s.IndexerEnv, err)
		data, err := t.ask(ctx, "Indexer DATA_DIR", s.IndexerData)
		if err != nil {
			return err
		}
		s.IndexerData = data
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s site) paths(ctx context.Context) (paths, error) {
	data, addr, envSocket, err := s.relayPaths()
	if err != nil {
		return paths{}, err
	}
	store, err := s.indexerData()
	if err != nil {
		return paths{}, err
	}
	unit := ""
	if out, err := exec.CommandContext(ctx, "systemctl", "cat", s.RelayUnit).CombinedOutput(); err == nil {
		unit = socketFromUnit(string(out))
	}
	socket := resolveSocket(envSocket, unit, data)
	return paths{RelayData: data, IndexerData: store, RelayAddr: addr, RelaySocket: socket}, nil
}

// relayPaths reads DATA_DIR and RELAY_ADDRESS from the relay env file.
// When that file cannot be read, it uses the answers from promptUnread.
func (s site) relayPaths() (data, addr, socket string, err error) {
	env, err := readEnv(s.RelayEnv)
	if err != nil {
		if strings.TrimSpace(s.RelayData) == "" {
			return "", "", "", err
		}
		addr = strings.TrimSpace(s.RelayAddr)
		if addr == "" {
			addr = "localhost:3334"
		}
		return s.RelayData, addr, s.SocketPath, nil
	}
	data = strings.TrimSpace(env["DATA_DIR"])
	if data == "" {
		return "", "", "", fmt.Errorf("%s: DATA_DIR is not set", s.RelayEnv)
	}
	addr = strings.TrimSpace(env["RELAY_ADDRESS"])
	if addr == "" {
		addr = "localhost:3334"
	}
	return data, addr, env["RELAY_SOCKET_PATH"], nil
}

func (s site) indexerData() (string, error) {
	env, err := readEnv(s.IndexerEnv)
	if err != nil {
		if strings.TrimSpace(s.IndexerData) == "" {
			return "", err
		}
		return s.IndexerData, nil
	}
	store := strings.TrimSpace(env["DATA_DIR"])
	if store == "" {
		return "", fmt.Errorf("%s: DATA_DIR is not set", s.IndexerEnv)
	}
	return store, nil
}

// resolveSocket is where relay v2 will listen. The previous relay has no socket.
// LAUNCH_RELAY_SOCKET wins, then RELAY_SOCKET_PATH in the env file, then the
// unit, then relay.sock in DATA_DIR.
func resolveSocket(envPath, unitPath, data string) string {
	if v, ok := os.LookupEnv("LAUNCH_RELAY_SOCKET"); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(envPath); v != "" {
		return v
	}
	if v := strings.TrimSpace(unitPath); v != "" {
		return v
	}
	return filepath.Join(data, "relay.sock")
}

// socketFromUnit returns the last RELAY_SOCKET_PATH in a systemctl cat dump.
func socketFromUnit(text string) string {
	var path string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "Environment=")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			field = strings.Trim(field, `"'`)
			key, val, ok := strings.Cut(field, "=")
			if !ok || key != "RELAY_SOCKET_PATH" {
				continue
			}
			path = strings.Trim(val, `"'`)
		}
	}
	return path
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
		return catalog.Signer{}, fmt.Errorf("%s SIGN_WITH: %w", envFile, redact(err))
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
	raw := strings.TrimSpace(env["RELAY_PUBKEY"])
	var key string
	if raw != "" {
		var err error
		key, err = relay.PubkeyHex(raw)
		if err != nil || len(key) != 64 {
			return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is not a public key")
		}
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
	if key == "" {
		key = signer.PubKey
	} else if signer.PubKey != key {
		return "", catalog.Signer{}, fmt.Errorf("SIGN_WITH signs as %s, not RELAY_PUBKEY", signer.PubKey)
	}
	switch {
	case want.key != "" && key != want.key:
		return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is %s, want %s", key, want.key)
	case want.key == "" && key == oldKey:
		return "", catalog.Signer{}, fmt.Errorf("RELAY_PUBKEY is still 78ce…")
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
		if err := t.wait(ctx, append([]string{"Edit " + envFile + ":"}, lines...)...); err != nil {
			return "", catalog.Signer{}, err
		}
		key, signer, err := checkRelayEnv(ctx, envFile, want)
		if err == nil {
			return key, signer, nil
		}
		t.say("  ! %v", err)
	}
}

// within reports whether path is dir or a file inside it.
func within(path, dir string) bool {
	path, err1 := filepath.Abs(path)
	dir, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

var secretText = regexp.MustCompile(`bunker://\S+|nsec1\S+`)

// redact hides a bunker URL or nsec. The catalog key never leaves hotbox.
func redact(err error) error {
	if err == nil {
		return nil
	}
	msg := secretText.ReplaceAllString(err.Error(), "…")
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func firstLine(out []byte, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if line == "" {
		return err.Error()
	}
	return line
}
