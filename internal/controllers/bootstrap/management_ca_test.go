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
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mgmt-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestResolveManagementCA_Precedence exercises the ADR 0011 PR-4 source order:
// override env var > in-cluster CAData > in-cluster CAFile > fail.
func TestResolveManagementCA_Precedence(t *testing.T) {
	overrideCA := testCAPEM(t)
	caDataCA := testCAPEM(t)
	caFileCA := testCAPEM(t)

	t.Run("override inline PEM wins over CAData", func(t *testing.T) {
		t.Setenv(ManagementAPICAFileEnv, string(overrideCA))
		out, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caDataCA}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !certsEqual(t, out, overrideCA) {
			t.Error("override inline PEM did not take precedence")
		}
	})

	t.Run("override file path wins over CAData", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(p, overrideCA, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(ManagementAPICAFileEnv, p)
		out, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caDataCA}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !certsEqual(t, out, overrideCA) {
			t.Error("override file did not take precedence")
		}
	})

	t.Run("CAData used when no override", func(t *testing.T) {
		out, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caDataCA}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !certsEqual(t, out, caDataCA) {
			t.Error("CAData was not used")
		}
	})

	t.Run("CAFile used when no override and no CAData", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(p, caFileCA, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAFile: p}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !certsEqual(t, out, caFileCA) {
			t.Error("CAFile was not used")
		}
	})

	t.Run("no source available fails with ErrManagementCAUnavailable", func(t *testing.T) {
		_, err := ResolveManagementCA(&rest.Config{})
		if !errors.Is(err, ErrManagementCAUnavailable) {
			t.Errorf("expected ErrManagementCAUnavailable, got %v", err)
		}
	})

	t.Run("nil config fails", func(t *testing.T) {
		_, err := ResolveManagementCA(nil)
		if !errors.Is(err, ErrManagementCAUnavailable) {
			t.Errorf("expected ErrManagementCAUnavailable, got %v", err)
		}
	})

	t.Run("override path that does not exist fails", func(t *testing.T) {
		t.Setenv(ManagementAPICAFileEnv, "/nonexistent/ca.pem")
		_, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caDataCA}})
		if err == nil {
			t.Error("expected error for a missing override file")
		}
	})

	t.Run("invalid override PEM fails (no silent fallback)", func(t *testing.T) {
		t.Setenv(ManagementAPICAFileEnv, "-----BEGIN CERTIFICATE-----\nnot-base64\n-----END CERTIFICATE-----\n")
		_, err := ResolveManagementCA(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: caDataCA}})
		if err == nil {
			t.Error("expected error for an invalid override PEM; must NOT fall back to CAData")
		}
	})
}

// certsEqual reports whether the canonical output contains the same certificate
// as want (comparing DER of the first CERTIFICATE block, since ResolveManagementCA
// re-encodes canonically).
func certsEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	gb, _ := pem.Decode(got)
	wb, _ := pem.Decode(want)
	if gb == nil || wb == nil {
		return false
	}
	return string(gb.Bytes) == string(wb.Bytes)
}
