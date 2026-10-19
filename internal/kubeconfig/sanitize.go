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

// Package kubeconfig sanitizes a workload kubeconfig pushed by a node before
// any management-cluster controller trusts it. See ADR 0011,
// section "Sanitizer (internal/kubeconfig, pure)", for the rule set (S0-S5).
// Only inline credentials and an inline CA are accepted; server and CA are
// pinned by the caller.
//
// This package is pure: no Kubernetes client, no controller-runtime, no I/O
// beyond parsing bytes already in memory. Every field name, credential shape
// and CA check below is a denylist/allowlist derived from client-go's own
// kubeconfig loader (tools/clientcmd/client_config.go) — the goal is that
// nothing client-go itself would honor as file-path credentials, exec
// plugins, auth-provider plugins, impersonation or insecure TLS survives
// Sanitize.
//
// Sanitize does not merely reject unsafe input: it REBUILDS the output from
// an allowlist (S5). "Valid" is defined as a fixed point of Sanitize, i.e.
// Sanitize(x) == x, never "passes a denylist" — see IsFixedPoint. S1-S4 are
// necessarily a denylist and cannot anticipate every future client-go field;
// S5 is the actual guarantee callers rely on.
package kubeconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// MaxSize is the S0 limit: raw input larger than this is rejected before
// clientcmd.Load is ever called. A real k0s/k3s admin kubeconfig with a
// single CA and client cert is a few KiB; 64 KiB leaves generous headroom
// while bounding the cost of parsing and PEM-decoding attacker-controlled
// bytes.
const MaxSize = 64 * 1024

// Sanitize validates raw against S0-S4 and, on success, returns the S5
// rebuilt kubeconfig: one cluster named clusterName with server set to
// expectedServer and the validated CA, one user "<clusterName>-admin", one
// context "<clusterName>-admin@<clusterName>" set as current-context.
//
// raw is never partially trusted: on any rejection, Sanitize returns a
// *RejectedError and no output. The returned error's Reason and Message
// never contain a byte of raw, a certificate, a key, or a token — safe to
// log or place in a status condition (root CLAUDE.md rule 2).
//
// expectedServer is caller-supplied (the KairosControlPlane's own view of
// the control-plane endpoint), never read from raw: S2 explicitly ignores
// the input "server" field.
//
// Sanitize is deterministic: the same raw, clusterName and expectedServer
// always produce byte-identical output. It does not check certificate
// expiry (S3 loads the cert/key pair, but expiry is a separate, publish-time
// concern — see CheckClientCertificateExpiry).
func Sanitize(raw []byte, clusterName, expectedServer string) ([]byte, error) {
	if clusterName == "" {
		return nil, reject(ReasonStructure, "cluster name must not be empty")
	}
	if expectedServer == "" {
		return nil, reject(ReasonStructure, "expected server must not be empty")
	}
	if _, err := url.Parse(expectedServer); err != nil {
		return nil, reject(ReasonStructure, "expected server is not a valid URL")
	}

	// S0: size, checked before any parsing.
	if len(raw) > MaxSize {
		return nil, reject(ReasonOversized, "input is %d bytes, exceeds the %d byte limit", len(raw), MaxSize)
	}

	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, reject(ReasonUnparseable, "input does not decode as a kubeconfig")
	}

	// S4's "extensions at any level" is not limited to cluster/user/context
	// entries: the top-level Config and its (deprecated) Preferences also
	// carry an Extensions map. Reject both explicitly, fail-closed, rather
	// than relying on S5's rebuild to merely drop them silently.
	if len(cfg.Extensions) != 0 {
		return nil, reject(ReasonUnsafeIdentity, "top-level extensions are not permitted")
	}
	if len(cfg.Preferences.Extensions) != 0 {
		return nil, reject(ReasonUnsafeIdentity, "preferences extensions are not permitted")
	}

	// S2 and S4 are applied to every entry the input carries, not only the
	// ones the current context references — an unsafe field in a
	// non-referenced entry is rejected just the same. This loop runs before
	// the S1 cardinality check below so that an unsafe field is reported as
	// such even when it co-occurs with a cardinality violation (e.g. a
	// second, unreferenced cluster entry that is itself unsafe).
	clusterCAs := make(map[string][]byte, len(cfg.Clusters))
	for _, name := range sortedKeys(cfg.Clusters) {
		ca, verr := validateClusterEntry(name, cfg.Clusters[name])
		if verr != nil {
			return nil, verr
		}
		clusterCAs[name] = ca
	}

	type credentials struct {
		clientCertificateData []byte
		clientKeyData         []byte
		token                 string
	}
	userCreds := make(map[string]credentials, len(cfg.AuthInfos))
	for _, name := range sortedKeys(cfg.AuthInfos) {
		certData, keyData, token, verr := validateUserEntry(name, cfg.AuthInfos[name])
		if verr != nil {
			return nil, verr
		}
		userCreds[name] = credentials{clientCertificateData: certData, clientKeyData: keyData, token: token}
	}

	for _, name := range sortedKeys(cfg.Contexts) {
		if verr := validateContextEntry(name, cfg.Contexts[name]); verr != nil {
			return nil, verr
		}
	}

	// S1: structure. Only after every entry has already cleared S2/S4 do we
	// require there to be exactly one of each, correctly cross-referenced.
	inputCluster, inputUser, inputContext, err := validateStructure(cfg)
	if err != nil {
		return nil, err
	}

	ca := clusterCAs[inputCluster]
	creds := userCreds[inputUser]
	_ = inputContext // only used for the S1 checks above; the output names are always rebuilt (S5/OQ-9).

	// S5: rebuild, never edit. The output is entirely new objects; nothing
	// from the input Cluster/AuthInfo/Context is copied across except the
	// already-validated CA bundle and credentials.
	out := clientcmdapi.NewConfig()
	userName := clusterName + "-admin"
	contextName := userName + "@" + clusterName

	out.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   expectedServer,
		CertificateAuthorityData: ca,
	}
	out.AuthInfos[userName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: creds.clientCertificateData,
		ClientKeyData:         creds.clientKeyData,
		Token:                 creds.token,
	}
	out.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: userName,
	}
	out.CurrentContext = contextName

	rebuilt, err := clientcmd.Write(*out)
	if err != nil {
		// out is entirely our own construction; a Write failure here is a
		// programming error in this package, not an input problem.
		return nil, fmt.Errorf("kubeconfig: internal error serializing sanitized output: %w", err)
	}
	return rebuilt, nil
}

// IsFixedPoint reports whether raw is already exactly the byte sequence
// Sanitize would produce for the same clusterName and expectedServer. This
// is the ADR's definition of "valid" (sanitize(x) == x): a caller never
// asks "does raw pass a denylist", it asks "is raw already canonical". A
// caller who already holds S5 output can use this to detect drift (staging
// content changed, or the expected endpoint changed) with a single call
// instead of separately parsing and diffing.
func IsFixedPoint(raw []byte, clusterName, expectedServer string) (bool, error) {
	out, err := Sanitize(raw, clusterName, expectedServer)
	if err != nil {
		return false, err
	}
	return bytes.Equal(raw, out), nil
}

// validateStructure implements S1: exactly one cluster, one user and one
// context; the context references both; current-context names it. It
// assumes every entry already cleared S2/S4 (Sanitize calls this only after
// that loop), so it only checks cardinality and cross-references, never
// field content.
func validateStructure(cfg *clientcmdapi.Config) (clusterName, userName, contextName string, err error) {
	if n := len(cfg.Clusters); n != 1 {
		return "", "", "", reject(ReasonStructure, "expected exactly one cluster entry, found %d", n)
	}
	if n := len(cfg.AuthInfos); n != 1 {
		return "", "", "", reject(ReasonStructure, "expected exactly one user entry, found %d", n)
	}
	if n := len(cfg.Contexts); n != 1 {
		return "", "", "", reject(ReasonStructure, "expected exactly one context entry, found %d", n)
	}
	for name := range cfg.Clusters {
		clusterName = name
	}
	for name := range cfg.AuthInfos {
		userName = name
	}
	var ctx *clientcmdapi.Context
	for name, c := range cfg.Contexts {
		contextName = name
		ctx = c
	}
	if ctx.Cluster != clusterName {
		return "", "", "", reject(ReasonStructure, "context %q references cluster %q, want the only cluster %q", contextName, ctx.Cluster, clusterName)
	}
	if ctx.AuthInfo != userName {
		return "", "", "", reject(ReasonStructure, "context %q references user %q, want the only user %q", contextName, ctx.AuthInfo, userName)
	}
	if cfg.CurrentContext != contextName {
		return "", "", "", reject(ReasonStructure, "current-context %q does not name the only context %q", cfg.CurrentContext, contextName)
	}
	return clusterName, userName, contextName, nil
}

// validateClusterEntry implements S2 for a single cluster entry: rejects
// every unsafe field, requires inline certificate-authority-data, and
// returns a CANONICAL re-encoding of it (see validateCABundle) once every
// PEM block in it is confirmed to be a CERTIFICATE that parses with x509,
// with at least one being a CA. The input "server" field is deliberately
// never read here (S2: "the input server is ignored").
func validateClusterEntry(name string, c *clientcmdapi.Cluster) ([]byte, error) {
	if c == nil {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: entry is empty", name)
	}
	if c.CertificateAuthority != "" {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: certificate-authority is a file path; only inline certificate-authority-data is accepted", name)
	}
	if c.InsecureSkipTLSVerify {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: insecure-skip-tls-verify is not permitted", name)
	}
	if c.ProxyURL != "" {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: proxy-url is not permitted", name)
	}
	if c.TLSServerName != "" {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: tls-server-name is not permitted", name)
	}
	if len(c.Extensions) != 0 {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: extensions are not permitted", name)
	}
	if len(c.CertificateAuthorityData) == 0 {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: certificate-authority-data is required", name)
	}
	canonicalCA, err := validateCABundle(c.CertificateAuthorityData)
	if err != nil {
		return nil, reject(ReasonUnsafeCluster, "cluster %q: %s", name, err)
	}
	return canonicalCA, nil
}

// validateCABundle implements the PEM/x509 half of S2: every block must be a
// CERTIFICATE that parses with x509, and at least one parsed certificate
// must be a CA. The error strings here are our own, fixed, structural
// descriptions — never the underlying x509/pem error text or any byte of
// data, which could in principle echo attacker-controlled input.
//
// It returns a CANONICAL re-encoding of the bundle: each parsed CERTIFICATE
// block's DER (block.Bytes), pem.Encode-d back out in order, discarding
// everything pem.Decode skipped to find those blocks (leading bytes, and
// anything between blocks). The input is node-supplied, so the raw bytes
// are never propagated into S5 output verbatim — only what this function
// re-derives from a successfully parsed certificate. A comment cannot make
// non-PEM bytes trailing the last block safe, so that case is still
// rejected outright, below, rather than silently dropped.
func validateCABundle(data []byte) ([]byte, error) {
	rest := data
	var canonical bytes.Buffer
	blocks := 0
	sawCA := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks++
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("certificate-authority-data contains a PEM block of type %q; only CERTIFICATE is permitted", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate-authority-data contains a PEM block that does not parse as an X.509 certificate")
		}
		if cert.IsCA {
			sawCA = true
		}
		canonical.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
	}
	if blocks == 0 {
		return nil, fmt.Errorf("certificate-authority-data does not contain any PEM blocks")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("certificate-authority-data contains trailing non-PEM data")
	}
	if !sawCA {
		return nil, fmt.Errorf("certificate-authority-data does not contain a CA certificate")
	}
	return canonical.Bytes(), nil
}

// validateUserEntry implements S4 (unsafe fields) and S3 (credential shape)
// for a single user entry. It returns the inline client certificate/key
// pair, or a bearer token, whichever the entry legitimately carries.
func validateUserEntry(name string, u *clientcmdapi.AuthInfo) (clientCertificateData, clientKeyData []byte, token string, err error) {
	if u == nil {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: entry is empty", name)
	}
	if u.TokenFile != "" {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: tokenFile is not permitted", name)
	}
	if u.ClientCertificate != "" {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: client-certificate is a file path; only inline client-certificate-data is accepted", name)
	}
	if u.ClientKey != "" {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: client-key is a file path; only inline client-key-data is accepted", name)
	}
	if u.Exec != nil {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: exec credential plugins are not permitted", name)
	}
	if u.AuthProvider != nil {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: auth-provider is not permitted", name)
	}
	if u.Username != "" || u.Password != "" {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: username/password basic auth is not permitted", name)
	}
	if u.Impersonate != "" || u.ImpersonateUID != "" || len(u.ImpersonateGroups) != 0 || len(u.ImpersonateUserExtra) != 0 {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: impersonation fields (as/as-uid/as-groups/as-user-extra) are not permitted", name)
	}
	if len(u.Extensions) != 0 {
		return nil, nil, "", reject(ReasonUnsafeIdentity, "user %q: extensions are not permitted", name)
	}

	hasCertPair := len(u.ClientCertificateData) != 0 || len(u.ClientKeyData) != 0
	hasToken := u.Token != ""
	switch {
	case hasCertPair && hasToken:
		return nil, nil, "", reject(ReasonCredentials, "user %q: exactly one of an inline client certificate/key pair or a token is required, found both", name)
	case hasCertPair:
		if len(u.ClientCertificateData) == 0 || len(u.ClientKeyData) == 0 {
			return nil, nil, "", reject(ReasonCredentials, "user %q: client-certificate-data and client-key-data must both be set", name)
		}
		if _, kpErr := tls.X509KeyPair(u.ClientCertificateData, u.ClientKeyData); kpErr != nil {
			return nil, nil, "", reject(ReasonCredentials, "user %q: client certificate and key do not form a valid pair", name)
		}
		return u.ClientCertificateData, u.ClientKeyData, "", nil
	case hasToken:
		return nil, nil, u.Token, nil
	default:
		return nil, nil, "", reject(ReasonCredentials, "user %q: neither an inline client certificate/key pair nor a token was found", name)
	}
}

// validateContextEntry implements the S4 unsafe-field check for a single
// context entry. Cluster/AuthInfo/Namespace are plain names, not credential
// or trust material; the only unsafe surface on a Context is Extensions.
func validateContextEntry(name string, c *clientcmdapi.Context) error {
	if c == nil {
		return reject(ReasonUnsafeIdentity, "context %q: entry is empty", name)
	}
	if len(c.Extensions) != 0 {
		return reject(ReasonUnsafeIdentity, "context %q: extensions are not permitted", name)
	}
	return nil
}

// sortedKeys returns the keys of m in ascending order, so that validation
// order (and therefore which entry's rejection is reported first) is
// deterministic across runs, independent of Go's randomized map iteration.
func sortedKeys[T any](m map[string]*T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
