package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/avast/apkverifier/signingblock"
)

func TestSignersFileParses(t *testing.T) {
	kinds := map[string]bool{}
	for _, rule := range storeSigners {
		kinds[rule.kind] = true
	}
	for _, kind := range []string{"stamp", "frosting", "dn"} {
		if !kinds[kind] {
			t.Errorf("no %s rule", kind)
		}
	}
	if _, err := parseSigners("cert abc"); err == nil {
		t.Error("a line without a label parsed")
	}
	if _, err := parseSigners("hash abc label"); err == nil {
		t.Error("an unknown kind parsed")
	}
}

func TestStoreSigner(t *testing.T) {
	own := newTestKey(t)
	fdroid := subjectCert(t, pkix.Name{
		CommonName: "FDroid", OrganizationalUnit: []string{"FDroid"}, Organization: []string{"fdroid.org"},
		Locality: []string{"ORG"}, Province: []string{"ORG"}, Country: []string{"UK"},
	})
	stamp := subjectCert(t, pkix.Name{CommonName: "stamp"})
	rules := append(slicesClone(storeSigners), signerRule{kind: "stamp", value: certHash(stamp), label: "test-store"})

	cases := []struct {
		name  string
		chain signing
		block *signingblock.VerificationResult
		label string
	}{
		{"own key", signing{own.cert}, nil, ""},
		{"f-droid build key", signing{fdroid}, nil, "fdroid"},
		{"f-droid key in the lineage", signing{fdroid, own.cert}, nil, "fdroid"},
		{"play frosting", signing{own.cert}, &signingblock.VerificationResult{
			Frosting: &signingblock.FrostingResult{KeySha256: strings.ToUpper("7c5ce2a4a6c14d78aa8ff605d0bfd7fc8be6855104a256a3a3cc9f80bd565188")},
		}, "google-play"},
		{"other frosting key", signing{own.cert}, &signingblock.VerificationResult{
			Frosting: &signingblock.FrostingResult{KeySha256: strings.Repeat("0", 64)},
		}, ""},
		{"source stamp", signing{own.cert}, &signingblock.VerificationResult{
			SourceStamp: &signingblock.SourceStampResult{Cert: stamp},
		}, "test-store"},
		{"f-droid key wins over a stamp", signing{fdroid}, &signingblock.VerificationResult{
			SourceStamp: &signingblock.SourceStampResult{Cert: stamp},
		}, "fdroid"},
	}
	for _, c := range cases {
		err := storeSigner(rules, c.chain, c.block)
		var store *thirdPartyError
		switch {
		case c.label == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.label != "" && (!errors.As(err, &store) || store.label != c.label):
			t.Errorf("%s: got %v, want %s", c.name, err, c.label)
		}
	}
}

func subjectCert(t *testing.T, subject pkix.Name) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: subject, NotBefore: time.Unix(0, 0), NotAfter: time.Now().AddDate(1, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func slicesClone(rules []signerRule) []signerRule {
	return append([]signerRule(nil), rules...)
}
