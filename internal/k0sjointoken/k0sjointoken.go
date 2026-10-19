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

// Package k0sjointoken implements the k0s controller-join token format (ADR
// 0013): the encoded join token a controller joins with, and the bootstrap-token
// Secret in the workload kube-system namespace that backs it.
//
// A join token is a kubeconfig with cluster and context "k0s" and user
// "controller-bootstrap", gzipped and base64-encoded. Its server is an existing
// controller's join API on :9443 and its bearer a bootstrap token
// "<id>.<secret>". This is what `k0s token create --role=controller` and
// `k0s token pre-shared` produce; testdata holds a token and Secret made by
// real k0s.
//
// The KairosControlPlane controller mints and encodes tokens; the bootstrap
// controller decodes them to check which token it is about to render. The
// package is stdlib and client-go only, so it needs no k8s.io/cluster-bootstrap
// and no k0s import.
//
// Nothing here logs. Callers must never log a token, an encoded value, or a
// Secret's token-secret; the ID is the public half and may be logged.
package k0sjointoken

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// JoinAPIPort is the port of the k0s controller join API. `k0s token
	// create` uses the same port (APISpec.K0sControlPlaneAPIAddress).
	JoinAPIPort = "9443"

	// ControllerBootstrapUser is the user name inside a controller join token.
	// The joiner refuses a controller token whose user is anything else.
	ControllerBootstrapUser = "controller-bootstrap"

	// kubeconfigName is the cluster and context name k0s uses in a join token.
	kubeconfigName = "k0s"

	// Bootstrap-token Secret layout (k8s.io/cluster-bootstrap/token/api).
	SecretNamePrefix             = "bootstrap-token-"
	IDKey                        = "token-id"
	SecretKey                    = "token-secret"
	ExpirationKey                = "expiration"
	DescriptionKey               = "description"
	UsageControllerJoinKey       = "usage-bootstrap-controller-join"
	LegacyUsageControllerJoinKey = "usage-controller-join"
	usageAuthenticationKey       = "usage-bootstrap-authentication"
	usageSigningKey              = "usage-bootstrap-signing"
	extraGroupsKey               = "auth-extra-groups"

	// Description is written into the bootstrap-token Secret so an operator
	// listing tokens can tell where it came from.
	Description = "k0s controller-join token minted by cluster-api-provider-kairos"

	idLength     = 6
	secretLength = 16
	charset      = "abcdefghijklmnopqrstuvwxyz0123456789"

	// maxKubeconfigBytes bounds the decompressed size of a token we decode. A
	// real one is about 1.5 KiB.
	maxKubeconfigBytes = 64 << 10
)

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9]{6}$`)
	tokenPattern = regexp.MustCompile(`^([a-z0-9]{6})\.([a-z0-9]{16})$`)
)

// BootstrapToken is the "<id>.<secret>" bearer inside a k0s join token. The
// fields are unexported and String and MarshalLog redact the secret half, so an
// accidental log call prints the ID only.
type BootstrapToken struct {
	id     string
	secret string
}

// NewBootstrapToken returns the token for an ID and secret in the bootstrap
// token format ([a-z0-9], 6 and 16 characters).
func NewBootstrapToken(id, secret string) (BootstrapToken, error) {
	m := tokenPattern.FindStringSubmatch(id + "." + secret)
	if m == nil {
		return BootstrapToken{}, errors.New("not a bootstrap token")
	}
	return BootstrapToken{id: m[1], secret: m[2]}, nil
}

// ID is the public half of the token. It may be logged.
func (t BootstrapToken) ID() string { return t.id }

// Secret is the private half. Never log it.
func (t BootstrapToken) Secret() string { return t.secret }

// Bearer returns the full "<id>.<secret>" value. Never log it.
func (t BootstrapToken) Bearer() string { return t.id + "." + t.secret }

func (t BootstrapToken) String() string { return t.id + ".<redacted>" }

// MarshalLog implements logr.Marshaler.
func (t BootstrapToken) MarshalLog() any { return t.String() }

// ValidID reports whether id has the bootstrap-token ID format.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Generate returns a new bootstrap token from crypto/rand over [a-z0-9]: 6
// characters of ID and 16 of secret, the format k0s and kubeadm use.
func Generate() (BootstrapToken, error) {
	id, err := randomString(idLength)
	if err != nil {
		return BootstrapToken{}, err
	}
	secret, err := randomString(secretLength)
	if err != nil {
		return BootstrapToken{}, err
	}
	return BootstrapToken{id: id, secret: secret}, nil
}

func randomString(n int) (string, error) {
	limit := big.NewInt(int64(len(charset)))
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate bootstrap token: %w", err)
		}
		out[i] = charset[v.Int64()]
	}
	return string(out), nil
}

// Encode builds a k0s controller join token: a kubeconfig with cluster and
// context "k0s" and user "controller-bootstrap", gzipped at BestCompression and
// base64-encoded with the standard alphabet (k0s pkg/token/kubeconfig.go
// GenerateKubeconfig, pkg/token/joinencode.go).
func Encode(server string, caPEM []byte, tok BootstrapToken) (string, error) {
	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{kubeconfigName: {
			Server:                   server,
			CertificateAuthorityData: caPEM,
		}},
		Contexts: map[string]*clientcmdapi.Context{kubeconfigName: {
			Cluster:  kubeconfigName,
			AuthInfo: ControllerBootstrapUser,
		}},
		CurrentContext: kubeconfigName,
		AuthInfos: map[string]*clientcmdapi.AuthInfo{ControllerBootstrapUser: {
			Token: tok.Bearer(),
		}},
	})
	if err != nil {
		return "", errors.New("encode k0s join token: cannot serialize the kubeconfig")
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	if _, err := gz.Write(kubeconfig); err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Decoded is a parsed k0s controller join token.
type Decoded struct {
	Server string
	CAData []byte
	User   string
	Token  BootstrapToken
}

// Decode reverses Encode. It also reads tokens minted by k0s itself, which is
// how the KCP recognises a token an older init node pushed. Errors never quote
// the input.
func Decode(encoded string) (Decoded, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return Decoded{}, errors.New("decode k0s join token: not base64")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return Decoded{}, errors.New("decode k0s join token: not gzip")
	}
	data, err := io.ReadAll(io.LimitReader(gz, maxKubeconfigBytes+1))
	if err != nil {
		return Decoded{}, errors.New("decode k0s join token: corrupt gzip stream")
	}
	if len(data) > maxKubeconfigBytes {
		return Decoded{}, errors.New("decode k0s join token: kubeconfig too large")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return Decoded{}, errors.New("decode k0s join token: not a kubeconfig")
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || kctx == nil {
		return Decoded{}, errors.New("decode k0s join token: no current context")
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cluster == nil {
		return Decoded{}, errors.New("decode k0s join token: context names an unknown cluster")
	}
	auth, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok || auth == nil {
		return Decoded{}, errors.New("decode k0s join token: context names an unknown user")
	}
	m := tokenPattern.FindStringSubmatch(auth.Token)
	if m == nil {
		return Decoded{}, errors.New("decode k0s join token: bearer is not a bootstrap token")
	}
	return Decoded{
		Server: cluster.Server,
		CAData: cluster.CertificateAuthorityData,
		User:   kctx.AuthInfo,
		Token:  BootstrapToken{id: m[1], secret: m[2]},
	}, nil
}

// NewSecret returns the workload kube-system Secret that backs tok, in the
// shape k0s gives its own controller tokens (pkg/token/manager.go
// RandomBootstrapSecret): the controller-join usage in both its current and
// legacy key, and nothing else. There is no authentication or signing usage and
// no extra group, so the token cannot be used against kube-apiserver at all.
func NewSecret(tok BootstrapToken, expires time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretNamePrefix + tok.id,
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			IDKey:                        []byte(tok.id),
			SecretKey:                    []byte(tok.secret),
			ExpirationKey:                []byte(expires.UTC().Format(time.RFC3339)),
			DescriptionKey:               []byte(Description),
			UsageControllerJoinKey:       []byte("true"),
			LegacyUsageControllerJoinKey: []byte("true"),
		},
	}
}

// IsControllerJoinOnly reports whether s is a bootstrap-token Secret that
// carries the controller-join usage and cannot authenticate to kube-apiserver.
// The KCP only extends, reuses or deletes Secrets that pass.
func IsControllerJoinOnly(s *corev1.Secret) bool {
	if s.Type != corev1.SecretTypeBootstrapToken {
		return false
	}
	ctrlJoin := string(s.Data[UsageControllerJoinKey]) == "true" ||
		string(s.Data[LegacyUsageControllerJoinKey]) == "true"
	if !ctrlJoin {
		return false
	}
	if string(s.Data[usageAuthenticationKey]) == "true" || string(s.Data[usageSigningKey]) == "true" {
		return false
	}
	return len(s.Data[extraGroupsKey]) == 0
}

// Expiry returns the expiration of a bootstrap-token Secret. ok is false when
// the key is missing or unparsable.
func Expiry(s *corev1.Secret) (time.Time, bool) {
	raw, present := s.Data[ExpirationKey]
	if !present || len(raw) == 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SecretKeyFor is the workload namespace/name of the Secret backing token id.
func SecretKeyFor(id string) types.NamespacedName {
	return types.NamespacedName{Namespace: metav1.NamespaceSystem, Name: SecretNamePrefix + id}
}
