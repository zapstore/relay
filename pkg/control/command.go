package control

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nbd-wtf/go-nostr"
)

// AuthKind is the kind of a signed admin command (NIP-98 HTTP auth).
const AuthKind = 27235

// signedSkew is how far a signed command's created_at may be from the relay clock.
const signedSkew = 60

// Ops that change the relay. Each needs the relay's own user or a signed admin event.
const (
	opBan          = "ban"
	opUnban        = "unban"
	opUnbanHash    = "unban-hash"
	opAllow        = "allow"
	opDisallow     = "disallow"
	opSeal         = "seal"
	opDeleteBundle = "delete-bundle"
	opEnrich       = "enrich"
	opCancel       = "cancel"
)

// Ops that only read. Any peer that can open the socket may send them.
const (
	opAllowed = "allowed"
	opJobs    = "jobs"
)

type spec struct {
	required []string
	optional []string
}

// writeOps is the argument schema of every write. The signed tags list the
// required arguments, then the optional ones, in this order.
// Keep in step with webapp/modules/admin/utils/command.ts.
var writeOps = map[string]spec{
	opBan:          {required: []string{"kind", "id", "reason"}},
	opUnban:        {required: []string{"kind", "id"}},
	opUnbanHash:    {required: []string{"hash"}},
	opAllow:        {required: []string{"id"}},
	opDisallow:     {required: []string{"id"}},
	opSeal:         {optional: []string{"filter", "from", "note"}},
	opDeleteBundle: {required: []string{"from", "to"}},
	opEnrich:       {required: []string{"app"}, optional: []string{"note"}},
	opCancel:       {required: []string{"job"}},
}

var (
	hex64  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digits = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)
	appID  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

// args is a write's arguments by name. Empty values are absent.
type args map[string]string

// checkArgs returns the first problem with a write's arguments.
func checkArgs(op string, a args) error {
	s, ok := writeOps[op]
	if !ok {
		return errors.New("unknown op")
	}
	for key := range a {
		if !slices.Contains(s.required, key) && !slices.Contains(s.optional, key) {
			return fmt.Errorf("%s does not take %s", op, key)
		}
	}
	for _, key := range s.required {
		if a[key] == "" {
			return fmt.Errorf("%s is required", key)
		}
	}
	kind, hasKind := a["kind"]
	if hasKind && kind != "nostr" && kind != "forge" && kind != "cert" {
		return errors.New("kind must be nostr, forge, or cert")
	}
	if id, ok := a["id"]; ok {
		if len(id) > 200 || strings.ContainsAny(id, " \t\r\n") {
			return errors.New("id is invalid")
		}
		if (op == opAllow || op == opDisallow || kind == "nostr") && !hex64.MatchString(id) {
			return errors.New("id must be a hex public key")
		}
		if kind == "cert" && !hex64.MatchString(id) {
			return errors.New("id must be a certificate sha256")
		}
	}
	if reason, ok := a["reason"]; ok && (utf8.RuneCountInString(reason) > 280 || strings.TrimSpace(reason) == "") {
		return errors.New("reason must be 1 to 280 characters")
	}
	if note, ok := a["note"]; ok && utf8.RuneCountInString(note) > 280 {
		return errors.New("note is longer than 280 characters")
	}
	if hash, ok := a["hash"]; ok && !hex64.MatchString(hash) {
		return errors.New("hash must be 64 hex characters")
	}
	if filter, ok := a["filter"]; ok && (len(filter) > 255 || strings.ContainsAny(filter, " \t\r\n")) {
		return errors.New("filter is invalid")
	}
	for _, key := range []string{"from", "to"} {
		if v, ok := a[key]; ok && !digits.MatchString(v) {
			return fmt.Errorf("%s must be a snapshot number", key)
		}
	}
	if _, hasFrom := a["from"]; op == opSeal && hasFrom {
		if _, hasFilter := a["filter"]; hasFilter {
			return errors.New("leave the filter empty when from is set")
		}
	}
	if app, ok := a["app"]; ok && !appID.MatchString(app) {
		return errors.New("app id is invalid")
	}
	if job, ok := a["job"]; ok && !digits.MatchString(job) {
		return errors.New("job is invalid")
	}
	return nil
}

// commandTags are the tags a signed command must carry, exactly and in order.
func commandTags(relay, op string, a args) nostr.Tags {
	s := writeOps[op]
	tags := nostr.Tags{{"u", "relay.sock"}, {"relay", relay}, {"op", op}}
	for _, key := range append(slices.Clone(s.required), s.optional...) {
		if v := a[key]; v != "" {
			tags = append(tags, nostr.Tag{key, v})
		}
	}
	return tags
}

// signedCommand reads the op and arguments out of a signed admin event.
// It does not check the signature, the key, the clock, or replay.
func signedCommand(relay string, event *nostr.Event) (string, args, error) {
	if event.Kind != AuthKind || event.Content != "" {
		return "", nil, errors.New("not an admin command event")
	}
	if len(event.Tags) < 3 || len(event.Tags[2]) != 2 || event.Tags[2][0] != "op" {
		return "", nil, errors.New("not an admin command event")
	}
	op := event.Tags[2][1]
	if _, ok := writeOps[op]; !ok {
		return "", nil, errors.New("unknown op")
	}
	a := args{}
	for _, tag := range event.Tags[3:] {
		if len(tag) != 2 {
			return "", nil, errors.New("malformed tag")
		}
		if _, dup := a[tag[0]]; dup {
			return "", nil, errors.New("duplicate tag")
		}
		a[tag[0]] = tag[1]
	}
	if err := checkArgs(op, a); err != nil {
		return "", nil, err
	}
	if !slices.EqualFunc(commandTags(relay, op, a), event.Tags, slices.Equal) {
		return "", nil, errors.New("signed tags do not match this relay's command")
	}
	return op, a, nil
}
