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

package kubeconfig_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubeconfig"
)

// fakeToken is a documentation-style, obviously-not-real bearer token used
// wherever a test case needs S3's token credential shape. It must never
// appear in any error message Sanitize returns.
const fakeToken = "eyJFWEFNUExFIjoidGhpcy1pcy1hLWZha2UtdG9rZW4tZm9yLXRlc3RzLW9ubHkifQ.NOT-A-REAL-TOKEN"

const testClusterName = "test-cluster"
const testExpectedServer = "https://198.51.100.10:6443"

// generateCertPEM returns a PEM-encoded self-signed certificate and its
// PEM-encoded EC private key. These are throwaway, freshly generated for
// each test process run — never real credentials.
func generateCertPEM(t *testing.T, isCA bool, notBefore, notAfter time.Time) (certPEM, keyPEM []byte, cert *x509.Certificate) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kubeconfig-sanitizer-test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, cert
}

// baseConfig returns a structurally valid, single cluster/user/context
// kubeconfig using a fresh self-signed CA and a client certificate signed by
// a different (also fresh) key, mirroring how k0s/k3s issue an admin
// kubeconfig whose client cert is not chained to the cluster CA (ADR 0011,
// S3 note: "No chaining to the kubeconfig CA").
func baseConfig(t *testing.T) *clientcmdapi.Config {
	t.Helper()
	caPEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	clientCertPEM, clientKeyPEM, _ := generateCertPEM(t, false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["source"] = &clientcmdapi.Cluster{
		Server:                   "https://node-supplied-and-must-be-ignored.invalid:6443",
		CertificateAuthorityData: caPEM,
	}
	cfg.AuthInfos["source-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: clientCertPEM,
		ClientKeyData:         clientKeyPEM,
	}
	cfg.Contexts["source-context"] = &clientcmdapi.Context{
		Cluster:  "source",
		AuthInfo: "source-admin",
	}
	cfg.CurrentContext = "source-context"
	return cfg
}

func rawOf(t *testing.T, cfg *clientcmdapi.Config) []byte {
	t.Helper()
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("marshal test kubeconfig: %v", err)
	}
	return raw
}

func rejectedReason(t *testing.T, err error) kubeconfig.RejectionReason {
	t.Helper()
	var rerr *kubeconfig.RejectedError
	if !errors.As(err, &rerr) {
		t.Fatalf("error is not a *kubeconfig.RejectedError: %v (%T)", err, err)
	}
	return rerr.Reason
}

func TestSanitize_TableDriven(t *testing.T) {
	type tc struct {
		name       string
		mutate     func(t *testing.T, cfg *clientcmdapi.Config)
		wantErr    bool
		wantReason kubeconfig.RejectionReason
		// forbidden are secret substrings that must never leak into the
		// error message when wantErr is true.
		forbidden []string
	}

	tests := []tc{
		{
			name:    "valid kubeconfig with inline client certificate is sanitized",
			mutate:  func(t *testing.T, cfg *clientcmdapi.Config) {},
			wantErr: false,
		},
		{
			name: "valid kubeconfig with bearer token is sanitized",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"] = &clientcmdapi.AuthInfo{Token: fakeToken}
			},
			wantErr: false,
		},

		// --- S0: oversized input, checked before clientcmd.Load. ---
		{
			name: "S0 oversized input is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].Server = strings.Repeat("A", kubeconfig.MaxSize+1)
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonOversized,
		},
		{
			name: "S0 oversized input carrying an unsafe field still fails closed",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].InsecureSkipTLSVerify = true
				cfg.Clusters["source"].Server = strings.Repeat("A", kubeconfig.MaxSize+1)
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonOversized,
		},

		// --- S1: structure. ---
		{
			name: "S1 a second cluster entry causes a structure rejection",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				caPEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
				cfg.Clusters["other"] = &clientcmdapi.Cluster{CertificateAuthorityData: caPEM}
			},
			wantErr: true,
			// Whichever rule fires first (S2 on "other" or S1 cardinality),
			// this input must be rejected. Reason asserted per-case below.
		},
		{
			name: "S1 current-context not matching the only context is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.CurrentContext = "does-not-exist"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonStructure,
		},
		{
			name: "S1 context referencing an unknown cluster is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Contexts["source-context"].Cluster = "does-not-exist"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonStructure,
		},
		{
			name: "S1 context referencing an unknown user is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Contexts["source-context"].AuthInfo = "does-not-exist"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonStructure,
		},

		// --- S2: cluster field safety. ---
		{
			name: "S2 insecure-skip-tls-verify is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].InsecureSkipTLSVerify = true
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 proxy-url is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].ProxyURL = "http://proxy.invalid:3128"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 tls-server-name is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].TLSServerName = "attacker.invalid"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 certificate-authority file path is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].CertificateAuthority = "/etc/kubernetes/pki/ca.crt"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
			forbidden:  []string{"/etc/kubernetes/pki/ca.crt"},
		},
		{
			name: "S2 missing certificate-authority-data is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].CertificateAuthorityData = nil
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 a non-CERTIFICATE PEM block in certificate-authority-data is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				_, keyPEM, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
				cfg.Clusters["source"].CertificateAuthorityData = keyPEM
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 certificate-authority-data with no CA certificate is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				leafPEM, _, _ := generateCertPEM(t, false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
				cfg.Clusters["source"].CertificateAuthorityData = leafPEM
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 trailing non-PEM data after a valid CA certificate is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Clusters["source"].CertificateAuthorityData = append(
					append([]byte{}, cfg.Clusters["source"].CertificateAuthorityData...),
					[]byte("garbage-that-is-not-pem")...,
				)
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},
		{
			name: "S2 a second, unreferenced cluster entry carrying an unsafe field is still rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				caPEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
				cfg.Clusters["other"] = &clientcmdapi.Cluster{
					CertificateAuthorityData: caPEM,
					InsecureSkipTLSVerify:    true,
				}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeCluster,
		},

		// --- S3: credential shape. ---
		{
			name: "S3 client-certificate-data without client-key-data is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].ClientKeyData = nil
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonCredentials,
		},
		{
			name: "S3 a client cert/key pair that does not load together is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				_, mismatchedKeyPEM, _ := generateCertPEM(t, false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
				cfg.AuthInfos["source-admin"].ClientKeyData = mismatchedKeyPEM
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonCredentials,
		},
		{
			name: "S3 both a cert pair and a token present is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].Token = fakeToken
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonCredentials,
			forbidden:  []string{fakeToken},
		},
		{
			name: "S3 neither a cert pair nor a token is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"] = &clientcmdapi.AuthInfo{}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonCredentials,
		},

		// --- S4: identity field safety. ---
		{
			name: "S4 tokenFile is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].TokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
			forbidden:  []string{"/var/run/secrets/kubernetes.io/serviceaccount/token"},
		},
		{
			name: "S4 exec is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].Exec = &clientcmdapi.ExecConfig{Command: "cat"}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 auth-provider is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "gcp"}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 client-certificate file path is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].ClientCertificate = "/etc/kubernetes/pki/admin.crt"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
			forbidden:  []string{"/etc/kubernetes/pki/admin.crt"},
		},
		{
			name: "S4 client-key file path is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].ClientKey = "/etc/kubernetes/pki/admin.key"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 username/password is rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].Username = "admin"
				cfg.AuthInfos["source-admin"].Password = "hunter2"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
			forbidden:  []string{"hunter2"},
		},
		{
			name: "S4 impersonation fields are rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["source-admin"].Impersonate = "cluster-admin"
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 a second, unreferenced user entry carrying tokenFile is still rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.AuthInfos["other"] = &clientcmdapi.AuthInfo{TokenFile: "/tmp/whatever"}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 a second, unreferenced context entry carrying extensions is still rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Contexts["other"] = &clientcmdapi.Context{
					Cluster:    "source",
					AuthInfo:   "source-admin",
					Extensions: map[string]runtime.Object{"foo": &runtime.Unknown{Raw: []byte("{}")}},
				}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},

		// --- S4: top-level extensions ("extensions at any level"). ---
		{
			name: "S4 top-level Config extensions are rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Extensions = map[string]runtime.Object{"foo": &runtime.Unknown{Raw: []byte("{}")}}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
		{
			name: "S4 Preferences extensions are rejected",
			mutate: func(t *testing.T, cfg *clientcmdapi.Config) {
				cfg.Preferences.Extensions = map[string]runtime.Object{"foo": &runtime.Unknown{Raw: []byte("{}")}}
			},
			wantErr:    true,
			wantReason: kubeconfig.ReasonUnsafeIdentity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig(t)
			tt.mutate(t, cfg)
			raw := rawOf(t, cfg)

			out, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(out) == 0 {
					t.Fatalf("expected non-empty sanitized output")
				}
				return
			}

			if err == nil {
				t.Fatalf("expected an error, got none (output len=%d)", len(out))
			}
			if out != nil {
				t.Fatalf("expected nil output on error, got %d bytes", len(out))
			}
			if tt.wantReason != "" {
				if got := rejectedReason(t, err); got != tt.wantReason {
					t.Fatalf("reason = %q, want %q (err: %v)", got, tt.wantReason, err)
				}
			} else {
				var rerr *kubeconfig.RejectedError
				if !errors.As(err, &rerr) {
					t.Fatalf("error is not a *kubeconfig.RejectedError: %v", err)
				}
			}
			for _, secret := range tt.forbidden {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error message leaked secret material: %q contains %q", err.Error(), secret)
				}
			}
		})
	}
}

// TestSanitize_ClusterExtensionsRejected exercises S2's extensions check
// with a real (empty-but-present) extensions map, since
// map[string]runtime.Object needs a concrete runtime.Object type which the
// table above cannot express inline.
func TestSanitize_ClusterExtensionsRejected(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Clusters["source"].Extensions = map[string]runtime.Object{
		"foo": &runtime.Unknown{Raw: []byte("{}")},
	}
	raw := rawOf(t, cfg)

	_, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err == nil {
		t.Fatal("expected an error for cluster extensions")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonUnsafeCluster {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonUnsafeCluster)
	}
}

// TestSanitize_UserExtensionsRejected mirrors
// TestSanitize_ClusterExtensionsRejected for S4's "extensions at any level"
// on the referenced user entry itself (as opposed to the table's
// second/unreferenced-entry case).
func TestSanitize_UserExtensionsRejected(t *testing.T) {
	cfg := baseConfig(t)
	cfg.AuthInfos["source-admin"].Extensions = map[string]runtime.Object{
		"foo": &runtime.Unknown{Raw: []byte("{}")},
	}
	raw := rawOf(t, cfg)

	_, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err == nil {
		t.Fatal("expected an error for user extensions")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonUnsafeIdentity {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonUnsafeIdentity)
	}
}

func TestSanitize_S0_EmptyClusterName(t *testing.T) {
	raw := rawOf(t, baseConfig(t))
	_, err := kubeconfig.Sanitize(raw, "", testExpectedServer)
	if err == nil {
		t.Fatal("expected an error for empty cluster name")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonStructure {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonStructure)
	}
}

func TestSanitize_S0_EmptyExpectedServer(t *testing.T) {
	raw := rawOf(t, baseConfig(t))
	_, err := kubeconfig.Sanitize(raw, testClusterName, "")
	if err == nil {
		t.Fatal("expected an error for empty expected server")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonStructure {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonStructure)
	}
}

func TestSanitize_MalformedExpectedServer(t *testing.T) {
	raw := rawOf(t, baseConfig(t))
	// "%zz" is an invalid percent-escape, which url.Parse rejects outright.
	_, err := kubeconfig.Sanitize(raw, testClusterName, "https://example.invalid/%zz")
	if err == nil {
		t.Fatal("expected an error for a malformed expected server URL")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonStructure {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonStructure)
	}
}

func TestSanitize_Unparseable(t *testing.T) {
	_, err := kubeconfig.Sanitize([]byte("not a kubeconfig at all: {{{"), testClusterName, testExpectedServer)
	if err == nil {
		t.Fatal("expected an error for unparseable input")
	}
	if got := rejectedReason(t, err); got != kubeconfig.ReasonUnparseable {
		t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonUnparseable)
	}
}

// TestSanitize_S5_RebuildsCanonicalShape asserts the S5 output shape: one
// cluster named <c> with server = expected and the validated CA, one user
// "<c>-admin", one context "<c>-admin@<c>" set as current-context, and that
// the node-supplied server and input names never survive.
func TestSanitize_S5_RebuildsCanonicalShape(t *testing.T) {
	cfg := baseConfig(t)
	raw := rawOf(t, cfg)

	out, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}

	rebuilt, err := clientcmd.Load(out)
	if err != nil {
		t.Fatalf("output does not parse as a kubeconfig: %v", err)
	}

	wantUser := testClusterName + "-admin"
	wantContext := wantUser + "@" + testClusterName

	if len(rebuilt.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(rebuilt.Clusters))
	}
	cluster, ok := rebuilt.Clusters[testClusterName]
	if !ok {
		t.Fatalf("cluster %q not found in output", testClusterName)
	}
	if cluster.Server != testExpectedServer {
		t.Fatalf("server = %q, want %q (input server must be ignored)", cluster.Server, testExpectedServer)
	}
	if len(rebuilt.AuthInfos) != 1 {
		t.Fatalf("users = %d, want 1", len(rebuilt.AuthInfos))
	}
	if _, ok := rebuilt.AuthInfos[wantUser]; !ok {
		t.Fatalf("user %q not found in output", wantUser)
	}
	if len(rebuilt.Contexts) != 1 {
		t.Fatalf("contexts = %d, want 1", len(rebuilt.Contexts))
	}
	if _, ok := rebuilt.Contexts[wantContext]; !ok {
		t.Fatalf("context %q not found in output", wantContext)
	}
	if rebuilt.CurrentContext != wantContext {
		t.Fatalf("current-context = %q, want %q", rebuilt.CurrentContext, wantContext)
	}
	if _, ok := rebuilt.Clusters["source"]; ok {
		t.Fatalf("input cluster name %q survived into the output", "source")
	}
}

// TestSanitize_CABundleCanonicalized asserts that S2/S5 never propagate raw
// certificate-authority-data bytes verbatim: the output CA bundle is a
// canonical re-encoding of the DER of each parsed CERTIFICATE block, so
// leading and inter-block bytes that pem.Decode skips over (which are never
// validated, and are node-supplied) do not survive into the contract
// kubeconfig, and the canonicalized output is itself a fixed point.
func TestSanitize_CABundleCanonicalized(t *testing.T) {
	cert1PEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	cert2PEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))

	var garbage bytes.Buffer
	garbage.WriteString("LEADING GARBAGE THAT IS NOT PEM AT ALL\n")
	garbage.Write(cert1PEM)
	garbage.WriteString("INTER-BLOCK GARBAGE THAT IS ALSO NOT PEM\n")
	garbage.Write(cert2PEM)

	cfg := baseConfig(t)
	cfg.Clusters["source"].CertificateAuthorityData = garbage.Bytes()
	raw := rawOf(t, cfg)

	out, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}

	rebuilt, err := clientcmd.Load(out)
	if err != nil {
		t.Fatalf("output does not parse as a kubeconfig: %v", err)
	}
	gotCA := rebuilt.Clusters[testClusterName].CertificateAuthorityData

	var wantCA bytes.Buffer
	wantCA.Write(cert1PEM)
	wantCA.Write(cert2PEM)
	if string(gotCA) != wantCA.String() {
		t.Fatalf("CA bundle was not canonicalized:\n--- got ---\n%s\n--- want ---\n%s", gotCA, wantCA.String())
	}
	if strings.Contains(string(gotCA), "GARBAGE") {
		t.Fatalf("garbage bytes leaked into the output CA bundle: %s", gotCA)
	}

	fixed, err := kubeconfig.IsFixedPoint(out, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("IsFixedPoint(canonicalized output): %v", err)
	}
	if !fixed {
		t.Fatal("canonicalized output is not a fixed point of Sanitize")
	}
}

// TestSanitize_Deterministic asserts that sanitizing the same input twice,
// with the same arguments, produces byte-identical output.
func TestSanitize_Deterministic(t *testing.T) {
	raw := rawOf(t, baseConfig(t))

	out1, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (1st): %v", err)
	}
	out2, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (2nd): %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("Sanitize is not deterministic:\n--- 1st ---\n%s\n--- 2nd ---\n%s", out1, out2)
	}
}

// TestSanitize_Idempotent asserts Sanitize(Sanitize(x)) == Sanitize(x): once
// sanitized, re-sanitizing with the same clusterName/expectedServer must be
// a no-op.
func TestSanitize_Idempotent(t *testing.T) {
	raw := rawOf(t, baseConfig(t))

	once, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (once): %v", err)
	}
	twice, err := kubeconfig.Sanitize(once, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (twice): %v", err)
	}
	if string(once) != string(twice) {
		t.Fatalf("Sanitize is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// TestSanitize_IdempotentWithToken exercises the same fixed-point property
// on the token credential shape, not just the certificate one.
func TestSanitize_IdempotentWithToken(t *testing.T) {
	cfg := baseConfig(t)
	cfg.AuthInfos["source-admin"] = &clientcmdapi.AuthInfo{Token: fakeToken}
	raw := rawOf(t, cfg)

	once, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (once): %v", err)
	}
	twice, err := kubeconfig.Sanitize(once, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize (twice): %v", err)
	}
	if string(once) != string(twice) {
		t.Fatalf("Sanitize is not idempotent for token credentials")
	}
}

func TestIsFixedPoint(t *testing.T) {
	raw := rawOf(t, baseConfig(t))

	sanitized, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}

	fixed, err := kubeconfig.IsFixedPoint(sanitized, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("IsFixedPoint(sanitized): %v", err)
	}
	if !fixed {
		t.Fatal("IsFixedPoint(Sanitize(x)) = false, want true")
	}

	fixed, err = kubeconfig.IsFixedPoint(raw, testClusterName, testExpectedServer)
	if err != nil {
		t.Fatalf("IsFixedPoint(raw): %v", err)
	}
	if fixed {
		t.Fatal("IsFixedPoint(raw) = true, want false (raw is not yet canonical)")
	}

	// A malicious/unsafe input is never a fixed point: IsFixedPoint must
	// surface the rejection, not silently report false.
	cfg := baseConfig(t)
	cfg.AuthInfos["source-admin"].TokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	unsafeRaw := rawOf(t, cfg)
	if _, err := kubeconfig.IsFixedPoint(unsafeRaw, testClusterName, testExpectedServer); err == nil {
		t.Fatal("expected IsFixedPoint to return an error for unsafe input")
	}
}

func TestCheckClientCertificateExpiry(t *testing.T) {
	buildWithValidity := func(t *testing.T, notBefore, notAfter time.Time) []byte {
		t.Helper()
		caPEM, _, _ := generateCertPEM(t, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
		clientCertPEM, clientKeyPEM, _ := generateCertPEM(t, false, notBefore, notAfter)
		cfg := clientcmdapi.NewConfig()
		cfg.Clusters[testClusterName] = &clientcmdapi.Cluster{Server: testExpectedServer, CertificateAuthorityData: caPEM}
		cfg.AuthInfos[testClusterName+"-admin"] = &clientcmdapi.AuthInfo{
			ClientCertificateData: clientCertPEM,
			ClientKeyData:         clientKeyPEM,
		}
		cfg.Contexts[testClusterName+"-admin@"+testClusterName] = &clientcmdapi.Context{
			Cluster:  testClusterName,
			AuthInfo: testClusterName + "-admin",
		}
		cfg.CurrentContext = testClusterName + "-admin@" + testClusterName
		return rawOf(t, cfg)
	}

	t.Run("not expired", func(t *testing.T) {
		raw := buildWithValidity(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
		if err := kubeconfig.CheckClientCertificateExpiry(raw, time.Now()); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		raw := buildWithValidity(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
		err := kubeconfig.CheckClientCertificateExpiry(raw, time.Now())
		if err == nil {
			t.Fatal("expected an expiry error")
		}
		if got := rejectedReason(t, err); got != kubeconfig.ReasonExpired {
			t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonExpired)
		}
	})

	t.Run("token auth has nothing to expire", func(t *testing.T) {
		cfg := clientcmdapi.NewConfig()
		cfg.Clusters[testClusterName] = &clientcmdapi.Cluster{Server: testExpectedServer}
		cfg.AuthInfos[testClusterName+"-admin"] = &clientcmdapi.AuthInfo{Token: fakeToken}
		cfg.Contexts[testClusterName+"-admin@"+testClusterName] = &clientcmdapi.Context{
			Cluster:  testClusterName,
			AuthInfo: testClusterName + "-admin",
		}
		cfg.CurrentContext = testClusterName + "-admin@" + testClusterName
		raw := rawOf(t, cfg)

		if err := kubeconfig.CheckClientCertificateExpiry(raw, time.Now()); err != nil {
			t.Fatalf("expected no error for token auth, got %v", err)
		}
	})

	t.Run("oversized input is rejected", func(t *testing.T) {
		huge := make([]byte, kubeconfig.MaxSize+1)
		err := kubeconfig.CheckClientCertificateExpiry(huge, time.Now())
		if err == nil {
			t.Fatal("expected an error for oversized input")
		}
		if got := rejectedReason(t, err); got != kubeconfig.ReasonOversized {
			t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonOversized)
		}
	})

	t.Run("unparseable input is rejected", func(t *testing.T) {
		err := kubeconfig.CheckClientCertificateExpiry([]byte("not a kubeconfig: {{{"), time.Now())
		if err == nil {
			t.Fatal("expected an error for unparseable input")
		}
		if got := rejectedReason(t, err); got != kubeconfig.ReasonUnparseable {
			t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonUnparseable)
		}
	})

	t.Run("input with more than one user is rejected", func(t *testing.T) {
		cfg := clientcmdapi.NewConfig()
		cfg.AuthInfos["a"] = &clientcmdapi.AuthInfo{Token: fakeToken}
		cfg.AuthInfos["b"] = &clientcmdapi.AuthInfo{Token: fakeToken}
		raw := rawOf(t, cfg)

		err := kubeconfig.CheckClientCertificateExpiry(raw, time.Now())
		if err == nil {
			t.Fatal("expected an error for more than one user entry")
		}
		if got := rejectedReason(t, err); got != kubeconfig.ReasonStructure {
			t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonStructure)
		}
	})

	t.Run("client-certificate-data with no PEM block is rejected", func(t *testing.T) {
		cfg := clientcmdapi.NewConfig()
		cfg.AuthInfos[testClusterName+"-admin"] = &clientcmdapi.AuthInfo{
			ClientCertificateData: []byte("not a pem block"),
			ClientKeyData:         []byte("not a pem block either"),
		}
		raw := rawOf(t, cfg)

		err := kubeconfig.CheckClientCertificateExpiry(raw, time.Now())
		if err == nil {
			t.Fatal("expected an error for a client certificate with no PEM block")
		}
		if got := rejectedReason(t, err); got != kubeconfig.ReasonCredentials {
			t.Fatalf("reason = %q, want %q", got, kubeconfig.ReasonCredentials)
		}
	})

	t.Run("Sanitize itself never rejects on expiry", func(t *testing.T) {
		cfg := baseConfig(t)
		expiredCertPEM, expiredKeyPEM, _ := generateCertPEM(t, false, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
		cfg.AuthInfos["source-admin"] = &clientcmdapi.AuthInfo{
			ClientCertificateData: expiredCertPEM,
			ClientKeyData:         expiredKeyPEM,
		}
		raw := rawOf(t, cfg)

		if _, err := kubeconfig.Sanitize(raw, testClusterName, testExpectedServer); err != nil {
			t.Fatalf("Sanitize must not reject an expired-but-otherwise-valid cert/key pair: %v", err)
		}
	})
}
