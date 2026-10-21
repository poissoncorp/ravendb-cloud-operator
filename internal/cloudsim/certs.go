package cloudsim

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// certificateZip builds the archive the RavenDB Cloud certificate endpoint
// serves, in the same layout, where <host> is the cluster's part of its
// hostnames:
//
//	PASSWORD.TXT
//	README.TXT
//	<host>.client.certificate.<date>.pfx
//	<host>.client.certificate.<date>.with.password.pfx
//	PEM/<host>.client.certificate.crt
//	PEM/<host>.client.certificate.key
//	PEM/<host>.client.certificate.pem
//
// The certificate is really generated, so a lab can run "openssl x509 -text"
// on what the operator published. Nothing trusts it.
func certificateZip(host, password string) ([]byte, error) {
	key, cert, err := newClientCertificate(host + ".client.certificate")
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	plainPFX, err := pkcs12.Modern.Encode(key, cert, nil, "")
	if err != nil {
		return nil, fmt.Errorf("encoding pfx: %w", err)
	}
	protectedPFX, err := pkcs12.Modern.Encode(key, cert, nil, password)
	if err != nil {
		return nil, fmt.Errorf("encoding protected pfx: %w", err)
	}

	stamp := time.Now().UTC().Format(time.DateOnly)
	return zipFiles([]zipFile{
		{"PASSWORD.TXT", []byte(password)},
		{"README.TXT", []byte(readmeText)},
		{fmt.Sprintf("%s.client.certificate.%s.pfx", host, stamp), plainPFX},
		{fmt.Sprintf("%s.client.certificate.%s.with.password.pfx", host, stamp), protectedPFX},
		{fmt.Sprintf("PEM/%s.client.certificate.crt", host), certPEM},
		{fmt.Sprintf("PEM/%s.client.certificate.key", host), keyPEM},
		{fmt.Sprintf("PEM/%s.client.certificate.pem", host), slices.Concat(certPEM, keyPEM)},
	})
}

// newClientCertificate generates a self-signed client certificate valid for a
// year.
func newClientCertificate(commonName string) (*rsa.PrivateKey, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generating serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"RavenDB Cloud lab mock"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing certificate: %w", err)
	}
	return key, cert, nil
}

type zipFile struct {
	name    string
	content []byte
}

func zipFiles(files []zipFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, fmt.Errorf("creating zip entry %q: %w", f.name, err)
		}
		if _, err := w.Write(f.content); err != nil {
			return nil, fmt.Errorf("writing zip entry %q: %w", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("closing zip: %w", err)
	}
	return buf.Bytes(), nil
}

const readmeText = `RavenDB Cloud lab mock
======================

This archive mimics the client certificate bundle served by RavenDB Cloud.
The certificate inside is generated on the fly by the lab mock and is trusted
by nothing. Do not use it for anything real.
`
