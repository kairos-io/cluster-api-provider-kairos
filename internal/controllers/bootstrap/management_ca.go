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
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/bootstrap"
)

// ManagementAPICAFileEnv is the operator override for the management API CA
// bundle nodes verify the node-push channel against (ADR 0011 PR-4, OQ-2). It
// mirrors the existing KAIROS_MANAGEMENT_API_OVERRIDE convention (main.go).
//
// The value is either:
//   - a filesystem PATH to a PEM bundle (the common case: a mounted CA file), or
//   - an inline PEM bundle (detected when the value contains a CERTIFICATE
//     block), for convenience where mounting a file is awkward.
//
// When set, it takes precedence over the in-cluster CA. There is deliberately NO
// insecure opt-out: an unreadable or invalid override fails the manager at
// startup rather than silently falling back to an unverified push (OQ-3).
const ManagementAPICAFileEnv = "KAIROS_MANAGEMENT_API_CA_FILE"

// ErrManagementCAUnavailable is returned when no management API CA can be
// resolved from either the operator override or the manager's in-cluster trust
// root, and the resolver signal for the same (a control-plane render that has a
// management endpoint but no CA to verify it against). The manager fails fast on
// it at startup; the reconciler maps the resolver's copy to
// ManagementCAUnavailableReason and requeues rather than handing out a push
// token for an unverifiable channel.
var ErrManagementCAUnavailable = errors.New("management API CA unavailable")

// ResolveManagementCA resolves and canonicalizes the CA bundle nodes use to
// verify the management API server's TLS certificate on the node-push channel
// (ADR 0011 PR-4). Precedence, highest first:
//
//  1. KAIROS_MANAGEMENT_API_CA_FILE (operator override: PEM path or inline PEM);
//  2. the manager's in-cluster rest.Config CAData (the management API server CA
//     the controller already trusts itself);
//  3. the manager's rest.Config CAFile.
//
// It NEVER uses the tenant workload cluster's kube-root-ca.crt: a tenant with
// ConfigMap write in the provider namespace could otherwise swap the CA and
// intercept the push (ADR 0011, "Where the management CA comes from").
//
// The returned bytes are the canonical, CERTIFICATE-only re-encoding produced by
// bootstrap.SanitizeManagementCABundle, safe to render into a write_files block.
// When no source yields a usable CA it returns ErrManagementCAUnavailable — the
// manager treats that as a hard startup failure (no insecure fallback exists).
func ResolveManagementCA(cfg *rest.Config) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(ManagementAPICAFileEnv)); v != "" {
		raw, err := readManagementCAOverride(v)
		if err != nil {
			return nil, err
		}
		out, err := bootstrap.SanitizeManagementCABundle(raw)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", ManagementAPICAFileEnv, v, err)
		}
		return out, nil
	}

	if cfg != nil && len(cfg.CAData) > 0 {
		out, err := bootstrap.SanitizeManagementCABundle(cfg.CAData)
		if err != nil {
			return nil, fmt.Errorf("in-cluster rest config CAData: %w", err)
		}
		return out, nil
	}

	if cfg != nil && cfg.CAFile != "" {
		raw, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%w: reading in-cluster rest config CAFile %q: %v", ErrManagementCAUnavailable, cfg.CAFile, err)
		}
		out, err := bootstrap.SanitizeManagementCABundle(raw)
		if err != nil {
			return nil, fmt.Errorf("in-cluster rest config CAFile %q: %w", cfg.CAFile, err)
		}
		return out, nil
	}

	return nil, ErrManagementCAUnavailable
}

// readManagementCAOverride returns the raw bytes of the operator override: the
// value verbatim when it is already inline PEM, otherwise the contents of the
// file it names.
func readManagementCAOverride(v string) ([]byte, error) {
	if strings.Contains(v, "-----BEGIN CERTIFICATE-----") {
		return []byte(v), nil
	}
	raw, err := os.ReadFile(v)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s=%q: %v", ErrManagementCAUnavailable, ManagementAPICAFileEnv, v, err)
	}
	return raw, nil
}
