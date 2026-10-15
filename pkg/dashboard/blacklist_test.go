package dashboard

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

func TestBlacklistTemplate(t *testing.T) {
	d, err := New(Config{}, rate.Limiter{}, nil, nil, relay.DB{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = d.template.ExecuteTemplate(&buf, "blacklist", blacklistPageData{
		IsAdmin: true,
		Publishers: []blacklistRow{{
			Kind: "forge", ID: "github.com/bad", Reason: "malware", Created: "2026-01-02",
		}},
		Hashes: []hashRow{{Hash: "abc", Reason: "malware"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{"github.com/bad", "malware", "abc", "Ban", "Remove"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}

	buf.Reset()
	err = d.template.ExecuteTemplate(&buf, "blacklist", blacklistPageData{
		IsAdmin:  true,
		ReadOnly: true,
		Publishers: []blacklistRow{{
			Kind: "forge", ID: "github.com/bad", Reason: "malware", Created: "2026-01-02",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = buf.String()
	if !strings.Contains(body, "read-only") {
		t.Fatalf("missing read-only notice\n%s", body)
	}
	if strings.Contains(body, "hx-post") {
		t.Fatalf("blacklist form still offered\n%s", body)
	}
}
