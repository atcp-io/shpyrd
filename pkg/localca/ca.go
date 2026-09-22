// Package localca manages a locally generated root certificate authority used
// by the local profile in place of a public or cloud CA. The CA is stored
// under a user directory (default ~/.shpyrd/ca) and can be installed in the
// operating system trust store so browsers accept certificates issued by
// cert-manager inside the cluster.
package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	certFile = "rootCA.pem"
	keyFile  = "rootCA-key.pem"
	validity = 10 * 365 * 24 * time.Hour
)

// CA is a root certificate and its private key in PEM form.
type CA struct {
	Dir     string
	CertPEM []byte
	KeyPEM  []byte
	Cert    *x509.Certificate
}

// DefaultDir returns ~/.shpyrd/ca.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".shpyrd", "ca"), nil
}

// LoadOrCreate returns the CA stored in dir, generating one on first use.
func LoadOrCreate(dir string) (*CA, bool, error) {
	ca, err := Load(dir)
	if err == nil {
		return ca, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	ca, err = Create(dir)
	if err != nil {
		return nil, false, err
	}
	return ca, true, nil
}

// Load reads an existing CA from dir.
func Load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, certFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM certificate", filepath.Join(dir, certFile))
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, certFile), err)
	}
	return &CA{Dir: dir, CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert}, nil
}

// Create generates a new ECDSA P-256 root CA valid for ten years.
func Create(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{"shpyrd development CA"},
			OrganizationalUnit: []string{host},
			CommonName:         "shpyrd development CA " + host,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        false,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{Dir: dir, CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert}, nil
}

// CertPath is the path of the CA certificate file.
func (c *CA) CertPath() string { return filepath.Join(c.Dir, certFile) }
