package relay

import (
	"strings"
	"testing"
)

func TestKnownKeysFileParses(t *testing.T) {
	keys, err := parseKnownKeys(knownKeysFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("known_keys.txt lists no key")
	}
	err = assetKnownKey(" FAC61745DC0903786FB9EDE62A962B399F7348F0BB6F899B8332667591033B9C ")
	if err == nil || !strings.Contains(err.Error(), "publicly known private key") || !strings.Contains(err.Error(), "debug.keystore") {
		t.Fatalf("the React Native debug certificate: %v", err)
	}
	if err := assetKnownKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("an unlisted certificate: %v", err)
	}
}

func TestParseKnownKeysRejectsBadLines(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for name, file := range map[string]string{
		"short hash": "abc  note",
		"upper case": strings.ToUpper(good) + "  note",
		"not hex":    strings.Repeat("zz", 32) + "  note",
		"no source":  good,
		"duplicate":  good + "  a\n" + good + "  b",
	} {
		if _, err := parseKnownKeys(file); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
