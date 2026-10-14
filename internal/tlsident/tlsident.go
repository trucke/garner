// Package tlsident gives the server its own TLS identity when no certificate
// is configured, and lets clients verify it by the public key's fingerprint
// (pin) instead of a certificate authority.
package tlsident

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	validity = 365 * 24 * time.Hour
	// renewBefore reissues the certificate this long before it expires. The
	// key stays the same, so clients' pins keep working.
	renewBefore = 30 * 24 * time.Hour
)

// Identity is a key and a self-signed certificate for the server's hosts.
type Identity struct {
	dir   string
	hosts []string
	key   *ecdsa.PrivateKey
	pin   string

	mu   sync.Mutex
	cert *tls.Certificate
}

// LoadOrCreate loads the key in dir or creates one, and makes sure the
// certificate covers hosts and is not about to expire.
func LoadOrCreate(dir string, hosts []string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	id := &Identity{dir: dir, hosts: hosts, key: key, pin: pinOf(pub)}
	if cert, err := id.loadCert(); err == nil && id.fresh(cert, time.Now()) {
		id.cert = cert
		return id, nil
	}
	if err := id.issue(time.Now()); err != nil {
		return nil, err
	}
	return id, nil
}

// Pin is the SHA-256 of the public key (SPKI), base64url-encoded.
func (id *Identity) Pin() string { return id.pin }

// GetCertificate serves the current certificate, renewing it with the same
// key when it nears expiry. Use it as tls.Config.GetCertificate.
func (id *Identity) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if !id.fresh(id.cert, time.Now()) {
		if err := id.issueLocked(time.Now()); err != nil {
			return nil, err
		}
	}
	return id.cert, nil
}

func (id *Identity) fresh(cert *tls.Certificate, now time.Time) bool {
	leaf := cert.Leaf
	if leaf == nil || now.Add(renewBefore).After(leaf.NotAfter) {
		return false
	}
	for _, h := range id.hosts {
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

func (id *Identity) issue(now time.Time) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.issueLocked(now)
}

func (id *Identity) issueLocked(now time.Time) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Garner"},
		// Backdate a little so small clock differences don't reject it.
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range id.hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &id.key.PublicKey, id.key)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(id.dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	id.cert = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.key, Leaf: leaf}
	return nil
}

func (id *Identity) loadCert() (*tls.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(id.dir, "cert.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("cert.pem holds no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || pinOf(pub) != id.pin {
		return nil, errors.New("certificate does not match the key")
	}
	return &tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: id.key, Leaf: leaf}, nil
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		return key, writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return key, nil
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pinOf(spki []byte) string {
	sum := sha256.Sum256(spki)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// PinMismatchError means the server presented a different key than the pinned
// one. Server is the pin of the key it presented.
type PinMismatchError struct{ Server string }

func (e *PinMismatchError) Error() string {
	return "server key does not match the pinned fingerprint"
}

// ClientConfig verifies a server by pin. It still checks that the certificate
// covers host (from the server URL) and is within its validity dates; only
// the certificate authority is replaced by the pin. host is passed in because
// TLS sends no server name for IP addresses.
func ClientConfig(pin, host string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Chain verification is replaced by VerifyConnection below.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			pub, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
			if err != nil {
				return err
			}
			if got := pinOf(pub); got != pin {
				return &PinMismatchError{Server: got}
			}
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return fmt.Errorf("server certificate is valid from %s to %s; check this machine's clock", leaf.NotBefore.Format(time.DateOnly), leaf.NotAfter.Format(time.DateOnly))
			}
			return leaf.VerifyHostname(host)
		},
	}
}
