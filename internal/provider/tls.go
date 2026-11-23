// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

// TLSConfig is a client's TLS for a remote API: the broker's client certificate and key
// (the key a secret file), and the server checked against ca_file. A CA there checks the
// server's chain and name; a server's own certificate that is no CA (Incus's, made for
// itself) is pinned: the server must present exactly that certificate. Without ca_file, the
// system's roots.
func TLSConfig(t config.TLS) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAFile != "" {
		data, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s: no certificate", t.CAFile)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.CAFile, err)
		}
		if cert.IsCA {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(data)
			tc.RootCAs = pool
		} else {
			// Pinned: the chain and name checks give way to the comparison.
			tc.InsecureSkipVerify = true
			tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || !bytes.Equal(raw[0], cert.Raw) {
					return errors.New("the server's certificate is not the pinned one")
				}
				return nil
			}
		}
	}
	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, config.Secret(t.KeyFile))
		if err != nil {
			return nil, fmt.Errorf("the client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}
