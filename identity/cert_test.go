package identity

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestCertificateCarriesIdentity(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := Certificate(priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("no DER")
	}
	if cert.Leaf == nil { // tls.Certificate parses the leaf lazily; force it
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := cert.Leaf.CheckSignatureFrom(cert.Leaf); err != nil {
		t.Fatalf("self-signature invalid: %v", err)
	}
	if cert.Leaf.Subject.CommonName != ID(pub) {
		t.Fatalf("CN = %q, want node id %q", cert.Leaf.Subject.CommonName, ID(pub))
	}
	if got := cert.Leaf.PublicKey.(ed25519.PublicKey); !got.Equal(pub) {
		t.Fatal("leaf public key is not the identity key")
	}
}

func TestVerifyPeerCertificate(t *testing.T) {
	_, routerPriv, _ := Generate()
	routerPub := PubString(routerPriv.Public().(ed25519.PublicKey))
	routerCert, err := Certificate(routerPriv)
	if err != nil {
		t.Fatal(err)
	}

	_, otherPriv, _ := Generate()
	otherCert, err := Certificate(otherPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := TLSClientConfig(routerCert, routerPub)
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("no verifier installed")
	}
	if err := cfg.VerifyPeerCertificate([][]byte{routerCert.Certificate[0]}, nil); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{otherCert.Certificate[0]}, nil); err == nil {
		t.Fatal("different identity accepted")
	}
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

var _ = tls.VersionTLS13 // keep tls imported for the config smoke below

func TestTLSConfigsShape(t *testing.T) {
	_, priv, _ := Generate()
	cert, _ := Certificate(priv)
	srv := TLSServerConfig(cert, PubString(priv.Public().(ed25519.PublicKey)))
	cli := TLSClientConfig(cert, PubString(priv.Public().(ed25519.PublicKey)))
	for _, c := range []*tls.Config{srv, cli} {
		if c.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %x, want TLS 1.3", c.MinVersion)
		}
		if len(c.NextProtos) != 1 || c.NextProtos[0] != "remote.quic" {
			t.Fatalf("NextProtos = %v", c.NextProtos)
		}
	}
}
