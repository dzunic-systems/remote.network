// cert.go: TLS certificates bound to node/router Ed25519 identities. QUIC
// requires TLS 1.3; we self-sign straight from the identity key so a QUIC
// handshake authenticates the same keypair the WSS mutual-challenge proved
// (certificate = identity, no PKI to operate).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"time"
)

// Certificate mints a fresh self-signed TLS certificate for an identity
// keypair. Short-lived on purpose: the identity is long-term, the cert is
// just a TLS wrapper for it and is re-minted per process (per listener).
func Certificate(priv ed25519.PrivateKey) (tls.Certificate, error) {
	pub := priv.Public().(ed25519.PublicKey)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: ID(pub)},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate clock skew
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true, // self-signed: the leaf is its own issuer
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, // SANs for strict TLS stacks
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// TLSServerConfig builds the QUIC server side: our identity cert, mandatory
// client certs, peer verification against wantPub (base32, the WSS-proven
// key — InsecureSkipVerify is safe because VerifyPeerCertificate does the
// real check).
func TLSServerConfig(ourCert tls.Certificate, wantPub string) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{ourCert},
		NextProtos:   []string{"remote.quic"},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return verifyConfig(ourCert, wantPub, true), nil
		},
	}
}

// TLSClientConfig builds the QUIC client side dialing a peer whose identity
// key must equal wantPub.
func TLSClientConfig(ourCert tls.Certificate, wantPub string) *tls.Config {
	return verifyConfig(ourCert, wantPub, false)
}

// TLSServerConfigAnyPeer is the reverse data-plane server side: any
// well-formed identity certificate is accepted at the TLS layer, and the
// caller identifies the peer afterwards (PeerIdentity on the connection
// state) and drops strangers. The punch listener can't use this — it knows
// exactly which key it expects — but the node-dialed plane accepts
// whichever authenticated session dials.
func TLSServerConfigAnyPeer(ourCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{ourCert},
		NextProtos:   []string{"remote.quic"},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errNoPeerCert
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			if _, ok := cert.PublicKey.(ed25519.PublicKey); !ok {
				return errNotEd25519
			}
			return nil
		},
	}
}

// PeerIdentity extracts the base32 identity key from a completed TLS
// handshake's peer certificate (ok=false when the peer presented none or a
// non-identity one).
func PeerIdentity(cs tls.ConnectionState) (string, bool) {
	if len(cs.PeerCertificates) == 0 {
		return "", false
	}
	edPub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", false
	}
	return PubString(edPub), true
}

func verifyConfig(ourCert tls.Certificate, wantPub string, server bool) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{ourCert},
		NextProtos:         []string{"remote.quic"},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verified below against the identity key
		ClientAuth:         tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errNoPeerCert
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			edPub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errNotEd25519
			}
			if PubString(edPub) != wantPub {
				return errKeyMismatch{want: wantPub, got: PubString(edPub)}
			}
			_ = server // both directions enforce the same rule
			return nil
		},
	}
}

type certError string

func (e certError) Error() string { return string(e) }

const (
	errNoPeerCert = certError("peer presented no certificate")
	errNotEd25519 = certError("peer certificate public key is not Ed25519")
)

type errKeyMismatch struct{ want, got string }

func (e errKeyMismatch) Error() string {
	return "peer identity mismatch: want " + Fingerprint(e.want) + " got " + Fingerprint(e.got)
}

// Fingerprint renders a base32 public key (or empty string) short for logs.
func Fingerprint(pubB32 string) string {
	if len(pubB32) <= 12 {
		return pubB32
	}
	return pubB32[:12] + "…"
}
