/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// makeTestCAPEM builds a valid self-signed CA certificate PEM for tests.
func makeTestCAPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// makeTestKeyPEM builds a PEM PRIVATE KEY block for the "no private key" test.
func makeTestKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestSanitizeManagementCABundle(t *testing.T) {
	validCA := makeTestCAPEM(t, "test-ca")
	secondCA := makeTestCAPEM(t, "test-ca-2")
	keyPEM := makeTestKeyPEM(t)

	t.Run("single certificate is accepted and round-trips", func(t *testing.T) {
		out, err := SanitizeManagementCABundle(validCA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(out), "-----BEGIN CERTIFICATE-----") {
			t.Errorf("output is not PEM: %q", out)
		}
		// The output must itself sanitize to the same bytes (idempotent).
		out2, err := SanitizeManagementCABundle(out)
		if err != nil {
			t.Fatalf("second pass errored: %v", err)
		}
		if string(out) != string(out2) {
			t.Errorf("SanitizeManagementCABundle is not idempotent")
		}
	})

	t.Run("multiple certificates are all retained", func(t *testing.T) {
		bundle := append(append([]byte{}, validCA...), secondCA...)
		out, err := SanitizeManagementCABundle(bundle)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := strings.Count(string(out), "-----BEGIN CERTIFICATE-----"); got != 2 {
			t.Errorf("expected 2 certificates, got %d", got)
		}
	})

	t.Run("empty input is rejected", func(t *testing.T) {
		if _, err := SanitizeManagementCABundle(nil); err == nil {
			t.Error("expected error for empty input")
		}
	})

	t.Run("a PRIVATE KEY block is rejected", func(t *testing.T) {
		bundle := append(append([]byte{}, validCA...), keyPEM...)
		_, err := SanitizeManagementCABundle(bundle)
		if err == nil {
			t.Fatal("expected error for a bundle containing a PRIVATE KEY block")
		}
		if !strings.Contains(err.Error(), "non-certificate") {
			t.Errorf("error should name the non-certificate block: %v", err)
		}
	})

	t.Run("trailing non-PEM content is rejected", func(t *testing.T) {
		bundle := append(append([]byte{}, validCA...), []byte("\ngarbage-not-pem\n")...)
		if _, err := SanitizeManagementCABundle(bundle); err == nil {
			t.Error("expected error for trailing non-PEM content")
		}
	})

	t.Run("non-PEM input is rejected", func(t *testing.T) {
		if _, err := SanitizeManagementCABundle([]byte("this is not a certificate")); err == nil {
			t.Error("expected error for non-PEM input")
		}
	})

	t.Run("a CERTIFICATE block with non-x509 bytes is rejected", func(t *testing.T) {
		bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-der")})
		if _, err := SanitizeManagementCABundle(bad); err == nil {
			t.Error("expected error for an unparseable certificate")
		}
	})

	t.Run("oversized input is rejected", func(t *testing.T) {
		big := make([]byte, maxManagementCABundleBytes+1)
		if _, err := SanitizeManagementCABundle(big); err == nil {
			t.Error("expected error for oversized input")
		}
	})
}
