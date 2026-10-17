package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay"
)

func TestPublisherTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	pub := strings.Repeat("a", 64)
	other := strings.Repeat("d", 64)
	cert := strings.Repeat("c", 64)
	proof := c1View{
		Cert: cert, Status: "Active", Role: "Owner", Other: other,
		Expiry: time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC),
	}
	page := publisherPage{
		Pubkey: pub,
		Publisher: publisherView{
			Pubkey: pub, Name: "Signal Foundation", NIP05: "signal@example.com", Website: "https://signal.org",
		},
		Apps: []pubApp{{ID: "org.signal.app", Name: "Signal", Updated: time.Now(), Cert: cert, Proof: &proof}},
		Extra: []c1View{{
			Cert: strings.Repeat("e", 64), Status: "Expired", Role: "Delegate", Other: pub,
			Apps: []c1App{{ID: "com.old.app", Name: "Old"}},
		}},
		CanBan: true,
		Now:    time.Now(),
	}
	body := render(t, d, "publisher", page)
	wantAll(t, body,
		"<title>Signal Foundation · Zapstore relay</title>",
		`href="/apps/org.signal.app"`,
		"Active", "Delegates to", "2027-03-04", cert,
		"Other certificates",
		`href="/apps/com.old.app"`, "Old",
		"https://npub.world/npub1",
		"Review ban",
		"/moderation?kind=nostr",
		"org.signal.app",
	)
	section := body[strings.Index(body, `id="pub-live"`):]
	poll := strings.Index(section, `class="live-poll"`)
	row := strings.Index(section, `class="row"`)
	if poll < 0 || row < 0 || row < poll+strings.Index(section[poll:], "</span>") {
		t.Fatal("app links inherit the publisher refresh selector")
	}

	page.Publisher.Banned = true
	page.CanBan = true
	if strings.Contains(render(t, d, "publisher", page), "Review ban") {
		t.Error("ban offered for a publisher already banned")
	}

	list := render(t, d, "publishers", publishersPage{
		Rows:  []publisherRow{{Pubkey: pub, Name: "Signal Foundation", Label: "Signal", Count: 1, Newest: time.Now()}},
		Total: 1, Now: time.Now(),
	})
	wantAll(t, list, `href="/publishers/`+pub+`"`, "Signal Foundation", "1 app")
	live := list[strings.Index(list, `id="pubs-live"`):]
	poll = strings.Index(live, `class="live-poll"`)
	row = strings.Index(live, `class="row"`)
	if poll < 0 || row < 0 || row < poll+strings.Index(live[poll:], "</span>") {
		t.Fatal("publisher links inherit the list refresh selector")
	}
}

func TestAssignProofs(t *testing.T) {
	cert := strings.Repeat("c", 64)
	other := strings.Repeat("e", 64)
	apps := []pubApp{{ID: "org.signal.app", Cert: cert}, {ID: "bare.app"}}
	proofs := []c1View{
		{Cert: cert, Status: "Expired"},
		{Cert: strings.ToUpper(cert), Status: "Active", Role: "Owner"},
		{Cert: other, Status: "Revoked", Role: "Delegate"},
	}
	apps, extra := assignProofs(apps, proofs)
	if apps[0].Proof == nil || apps[0].Proof.Status != "Active" || apps[0].Cert != cert {
		t.Fatalf("current signer %+v", apps[0].Proof)
	}
	if apps[1].Proof != nil || apps[1].Cert != "" {
		t.Fatalf("app without a certificate got %+v", apps[1])
	}
	if len(extra) != 1 || extra[0].Cert != other || extra[0].Status != "Revoked" {
		t.Fatalf("leftover %+v", extra)
	}
}

func TestSigningCerts(t *testing.T) {
	db := newRelayDB(t)
	pub := strings.Repeat("a", 64)
	older := strings.Repeat("b", 64)
	newer := strings.Repeat("c", 64)
	save := func(id string, at nostr.Timestamp, tags nostr.Tags) {
		t.Helper()
		if _, err := db.Save(t.Context(), &nostr.Event{
			ID: id, PubKey: pub, CreatedAt: at, Kind: events.KindAsset, Sig: "s", Tags: tags,
		}); err != nil {
			t.Fatal(err)
		}
	}
	save("asset-old", 10, nostr.Tags{{"i", "org.signal.app"}, {"apk_certificate_hash", older}})
	save("asset-new", 20, nostr.Tags{{"i", "org.signal.app"}, {"apk_certificate_hash", newer}})
	save("asset-other", 30, nostr.Tags{{"i", "com.other.app"}, {"apk_certificate_hash", older}})
	if _, err := db.Save(t.Context(), &nostr.Event{
		ID: "listing", PubKey: pub, CreatedAt: 1, Kind: events.KindApp, Sig: "s",
		Tags: nostr.Tags{{"d", "org.signal.app"}, {"name", "Signal"}},
	}); err != nil {
		t.Fatal(err)
	}
	d := newTestDashboard(t, Config{}, db)
	apps := []pubApp{{ID: "org.signal.app", Name: "Signal"}, {ID: "com.missing.app"}}
	if err := d.signingCerts(t.Context(), pub, apps); err != nil {
		t.Fatal(err)
	}
	if apps[0].Cert != newer || apps[1].Cert != "" {
		t.Fatalf("certs = %q %q", apps[0].Cert, apps[1].Cert)
	}
	extra := []c1View{{Cert: older}, {Cert: strings.Repeat("d", 64)}}
	if err := d.nameCertApps(t.Context(), extra); err != nil {
		t.Fatal(err)
	}
	if len(extra[0].Apps) != 2 || extra[0].Apps[0].Name != "com.other.app" || extra[0].Apps[1].ID != "org.signal.app" || extra[0].Apps[1].Name != "Signal" {
		t.Fatalf("older cert apps %+v", extra[0].Apps)
	}
	if len(extra[1].Apps) != 0 {
		t.Fatalf("unknown cert apps %+v", extra[1].Apps)
	}
}

func TestValidPubkey(t *testing.T) {
	if !validPubkey(strings.Repeat("a", 64)) {
		t.Fatal("hex pubkey rejected")
	}
	if validPubkey("npub1whatever") || validPubkey(strings.Repeat("A", 64)) || validPubkey(strings.Repeat("a", 63)) {
		t.Fatal("non-hex pubkey accepted")
	}
}
