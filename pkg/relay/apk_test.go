package relay

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSigningFollowsLineage(t *testing.T) {
	chain := readTestAPK(t, "v1v2v3-with-rsa-2048-lineage-3-signers.apk")
	if len(chain) != 3 {
		t.Fatalf("chain has %d certificates, want 3", len(chain))
	}
	oldest := pemHash(t, "rsa-2048.x509.pem")
	current := pemHash(t, "rsa-2048_3.x509.pem")
	if chain.signer() != current {
		t.Fatalf("signer %s, want %s", chain.signer(), current)
	}
	if chain.cert(oldest) == nil {
		t.Fatal("the oldest certificate is not in the lineage")
	}

	parsed, err := parseSigning(chain.der())
	if err != nil || parsed.signer() != current || len(parsed) != 3 {
		t.Fatalf("round trip: %v", err)
	}
}

func TestReadSigningWithoutLineage(t *testing.T) {
	chain := readTestAPK(t, "test-release.apk")
	if len(chain) != 1 {
		t.Fatalf("chain has %d certificates, want 1", len(chain))
	}
	if chain.cert(chain.signer()) == nil {
		t.Fatal("the signer is not in the chain")
	}
}

func readTestAPK(t *testing.T, name string) signing {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chain, err := readSigning(f)
	if err != nil {
		t.Fatal(err)
	}
	return chain
}

func pemHash(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("no pem block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certHash(cert)
}
