// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

func writeCert(t *testing.T, dir, name string, ca bool) (string, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: ca,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, der
}

func TestTLSConfigChecksACAOrPinsACertificate(t *testing.T) {
	dir := t.TempDir()
	ca, _ := writeCert(t, dir, "ca", true)
	tc, err := TLSConfig(config.TLS{CAFile: ca})
	if err != nil || tc.RootCAs == nil || tc.InsecureSkipVerify {
		t.Fatalf("a CA: %+v %v", tc, err)
	}
	leaf, der := writeCert(t, dir, "server", false)
	tc, err = TLSConfig(config.TLS{CAFile: leaf})
	if err != nil || !tc.InsecureSkipVerify || tc.VerifyPeerCertificate == nil {
		t.Fatalf("a pinned certificate: %v", err)
	}
	if err := tc.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("the pinned certificate was refused: %v", err)
	}
	_, other := writeCert(t, dir, "other", false)
	if err := tc.VerifyPeerCertificate([][]byte{other}, nil); err == nil {
		t.Fatal("another certificate was taken")
	}
	if _, err := TLSConfig(config.TLS{CertFile: leaf, KeyFile: filepath.Join(dir, "none.key")}); err == nil {
		t.Fatal("a client certificate without its key was taken")
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.pem"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfig(config.TLS{CAFile: filepath.Join(dir, "empty.pem")}); err == nil {
		t.Fatal("a file without a certificate was taken")
	}
}
