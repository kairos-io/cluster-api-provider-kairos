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

package kubeconfig

import (
	"encoding/pem"
	"time"

	"crypto/x509"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// CheckClientCertificateExpiry inspects the sole user entry's inline client
// certificate and reports whether it is outside its validity window as of
// now.
//
// This is deliberately NOT part of Sanitize (S3 loads the certificate/key
// pair with tls.X509KeyPair but never checks expiry): per ADR 0011, an
// expired certificate must never cause a contract kubeconfig to be deleted
// or withheld, only raise a condition (KubeconfigCertificateExpired). It is
// certificate expiry, not certificate shape, that is inevitable in normal
// operation (KD-62) and cannot be allowed to take a cluster's kubeconfig
// away. Callers invoke this function only at publish time, on content that
// has already passed Sanitize (or is already a fixed point of it), never as
// a substitute for Sanitize.
//
// sanitized is expected to be S5 output (or any input that is already a
// fixed point of Sanitize): exactly one user entry. A token-authenticated
// user has no certificate to expire and returns nil.
func CheckClientCertificateExpiry(sanitized []byte, now time.Time) error {
	if len(sanitized) > MaxSize {
		return reject(ReasonOversized, "input is %d bytes, exceeds the %d byte limit", len(sanitized), MaxSize)
	}
	cfg, err := clientcmd.Load(sanitized)
	if err != nil {
		return reject(ReasonUnparseable, "input does not decode as a kubeconfig")
	}
	if n := len(cfg.AuthInfos); n != 1 {
		return reject(ReasonStructure, "expected exactly one user entry, found %d", n)
	}
	var user *clientcmdapi.AuthInfo
	for _, u := range cfg.AuthInfos {
		user = u
	}
	if user == nil || len(user.ClientCertificateData) == 0 {
		// Token-authenticated: no certificate, nothing to expire.
		return nil
	}
	block, _ := pem.Decode(user.ClientCertificateData)
	if block == nil {
		return reject(ReasonCredentials, "client-certificate-data does not contain a PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return reject(ReasonCredentials, "client-certificate-data does not parse as an X.509 certificate")
	}
	if now.Before(cert.NotBefore) {
		return reject(ReasonExpired, "client certificate is not valid until %s", cert.NotBefore.Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return reject(ReasonExpired, "client certificate expired at %s", cert.NotAfter.Format(time.RFC3339))
	}
	return nil
}
