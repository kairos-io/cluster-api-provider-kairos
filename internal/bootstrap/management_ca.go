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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
)

// maxManagementCABundleBytes bounds the raw CA bundle accepted by
// SanitizeManagementCABundle. The management API server CA is a small handful of
// certificates; 64 KiB is generous headroom and matches the ADR 0011 sanitizer's
// S0 cap, so an oversized or garbage bundle fails fast instead of bloating every
// rendered cloud-config (and, for Metal3, the config-drive budget).
const maxManagementCABundleBytes = 64 * 1024

// SanitizeManagementCABundle validates and canonically re-encodes a PEM CA
// bundle for the node-push TLS trust root (ADR 0011 PR-4). The returned bytes
// are safe to write verbatim into a cloud-config write_files block: they contain
// only standard PEM CERTIFICATE blocks (base64 body + fixed headers, newline
// separated), so there is no YAML- or shell-metacharacter surface for a
// malicious CA to break out of.
//
// Rules (mirror the ADR "Templates" render-time checks):
//   - raw input at most 64 KiB;
//   - every PEM block is a CERTIFICATE that parses with crypto/x509 — a PRIVATE
//     KEY, or any other block type, is rejected (we never render a private key
//     into userdata, and a key here is a sign the wrong file was supplied);
//   - no trailing non-PEM content after the last block;
//   - at least one CERTIFICATE is present;
//   - the output is REBUILT from the parsed certificates (cert.Raw re-encoded),
//     so any bytes the parser ignored — leading preamble, comments, odd
//     inter-block headers — are dropped rather than passed through.
func SanitizeManagementCABundle(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("management API CA bundle is empty")
	}
	if len(raw) > maxManagementCABundleBytes {
		return nil, fmt.Errorf("management API CA bundle is %d bytes; exceeds the %d-byte limit", len(raw), maxManagementCABundleBytes)
	}

	var out []byte
	rest := raw
	count := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("management API CA bundle contains a non-certificate PEM block %q; only CERTIFICATE blocks are allowed", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("management API CA bundle contains an unparseable certificate: %w", err)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		count++
	}

	// Reject trailing non-PEM content so a bundle of "<valid cert>\n<garbage>"
	// does not silently render only the certificate — surface the malformed input.
	if trailing := strings.TrimSpace(string(rest)); trailing != "" {
		return nil, fmt.Errorf("management API CA bundle contains trailing non-PEM content")
	}
	if count == 0 {
		return nil, fmt.Errorf("management API CA bundle contains no CERTIFICATE blocks")
	}
	return out, nil
}
