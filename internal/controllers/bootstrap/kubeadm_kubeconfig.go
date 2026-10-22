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
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubeconfig"
)

// This file holds the kubeadm worker path's workload-kubeconfig trust helpers:
// the <cluster>-ca cross-check verifyTrustChain uses, and the reader the
// bootstrap manager's workload ClusterCache loads `<cluster>-kubeconfig`
// through.
//
// Why the reader exists: verifyTrustChain checks the kubeconfig Secret, but the
// workload client that mints the bootstrap token comes from a ClusterCache,
// which reads the same Secret again, later, on its own, and hands the bytes to
// clientcmd.RESTConfigFromKubeConfig. The rule for every workload client this
// project builds is that it comes from the sanitizer's rebuilt (S5) kubeconfig:
// an inline credential and an inline CA, nothing that refers to a file, a
// plugin, a proxy or skipped TLS verification (ADR 0011). ClusterCache has
// exactly one input for the kubeconfig, clustercache.Options.SecretClient, so
// that reader is where the sanitizer goes.

// crossCheckClusterCA compares the kubeconfig's CA certificates with the
// <cluster>-ca Secret's tls.crt, read through caReader (UNCACHED, cert only). It
// reports true only when that Secret exists and its certificates equal the
// kubeconfig's. A missing Secret is not an error and reports false: Kamaji's
// default setup and an upstream-KCP user-supplied CA legitimately have no CAPI
// controller owner, so the Secret is neither required nor owner-checked (ADR
// 0010 P1 step 1). A present Secret that does not parse, or does not match, is
// a terminal error; a failed read is a bootstrapNotReadyError.
func crossCheckClusterCA(ctx context.Context, caReader client.Reader, cluster *clusterv1.Cluster, kubeconfigCADER [][]byte) (bool, error) {
	caKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-ca"}
	caSecret := &corev1.Secret{}
	if err := caReader.Get(ctx, caKey, caSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, newNotReady("WaitingForClusterCA", "waiting to cross-check the %s Secret: %v", caKey.Name, err)
	}
	crt, ok := caSecret.Data[clusterCASecretCrtKey]
	if !ok || len(crt) == 0 {
		return false, fmt.Errorf("the %s Secret has no %q: cannot cross-check the CA pin", caKey.Name, clusterCASecretCrtKey)
	}
	_, caDER, err := caCertHashesFromPEM(crt)
	if err != nil {
		return false, fmt.Errorf("the %s Secret has an unparseable %q: %v", caKey.Name, clusterCASecretCrtKey, err)
	}
	if !derCertSetsEqual(kubeconfigCADER, caDER) {
		return false, fmt.Errorf("the kubeconfig CA does not match the %s Secret's %q; inspect the Secret and re-pin if this change is expected",
			caKey.Name, clusterCASecretCrtKey)
	}
	return true, nil
}

// anchoredServer is the server a workload client for a trusted kubeconfig
// dials: the Cluster control-plane endpoint when the kubeconfig names that same
// endpoint (in whatever spelling), otherwise the kubeconfig's own server, which
// the caller has accepted only because its CA matched <cluster>-ca.
func anchoredServer(kubeconfigServer, clusterEndpoint string) string {
	if sameServer(kubeconfigServer, clusterEndpoint) {
		return clusterEndpoint
	}
	return kubeconfigServer
}

// trustedWorkloadKubeconfig returns the S5 output of a kubeadm workload
// cluster's kubeconfig, or an error. It applies the same two independent
// anchors as verifyTrustChain (the kubeconfig names the Cluster control-plane
// endpoint, or its CA matches <cluster>-ca), because ClusterCache reads the
// Secret at a different time than verifyTrustChain did, and then rebuilds the
// kubeconfig with the sanitizer. The output carries an inline credential and an
// inline CA only, and the anchored server.
func trustedWorkloadKubeconfig(ctx context.Context, caReader client.Reader, cluster *clusterv1.Cluster, kcData []byte) ([]byte, error) {
	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		return nil, fmt.Errorf("cluster %s/%s has no valid control-plane endpoint yet", cluster.Namespace, cluster.Name)
	}
	if len(kcData) == 0 {
		return nil, fmt.Errorf("the kubeconfig Secret of cluster %s/%s has no %q payload", cluster.Namespace, cluster.Name, workloadKubeconfigSecretKey)
	}
	server, caData, err := serverAndCAFromKubeconfig(kcData)
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig Secret of cluster %s/%s is not a valid kubeconfig: %v", cluster.Namespace, cluster.Name, err)
	}
	_, kubeconfigCADER, err := caCertHashesFromPEM(caData)
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig Secret of cluster %s/%s has an unparseable certificate-authority: %v", cluster.Namespace, cluster.Name, err)
	}
	wantServer := "https://" + cluster.Spec.ControlPlaneEndpoint.String()
	if !sameServer(server, wantServer) {
		checked, err := crossCheckClusterCA(ctx, caReader, cluster, kubeconfigCADER)
		if err != nil {
			return nil, err
		}
		if !checked {
			return nil, fmt.Errorf("the kubeconfig Secret of cluster %s/%s names a server other than the control-plane endpoint %q, and there is no %s-ca Secret to cross-check its CA against",
				cluster.Namespace, cluster.Name, wantServer, cluster.Name)
		}
	}
	sanitized, err := kubeconfig.Sanitize(kcData, cluster.Name, anchoredServer(server, wantServer))
	if err != nil {
		// The error names the failing rule, never a byte of the content.
		return nil, fmt.Errorf("the kubeconfig Secret of cluster %s/%s failed sanitization: %w", cluster.Namespace, cluster.Name, err)
	}
	return sanitized, nil
}

// errNotAKubeconfigRead is returned by SanitizingKubeconfigReader for any read
// other than a typed Get of a `<cluster>-kubeconfig` Secret.
var errNotAKubeconfigRead = errors.New("this reader serves only typed Gets of <cluster>-kubeconfig Secrets")

// sanitizingKubeconfigReader is a client.Reader that returns
// `<cluster>-kubeconfig` Secrets with their value replaced by
// trustedWorkloadKubeconfig's output. Any other read fails closed.
type sanitizingKubeconfigReader struct {
	// mgmt reads the kubeconfig Secret and the Cluster.
	mgmt client.Reader
	// caReader is an UNCACHED reader for the <cluster>-ca cross-check.
	caReader client.Reader
}

// NewSanitizingKubeconfigReader returns the reader main.go passes to the
// bootstrap manager's workload ClusterCache as clustercache.Options.SecretClient.
// mgmt is the (cached) management reader for the kubeconfig Secret and the
// Cluster; caReader is an UNCACHED management reader for the <cluster>-ca
// cross-check.
func NewSanitizingKubeconfigReader(mgmt, caReader client.Reader) client.Reader {
	return &sanitizingKubeconfigReader{mgmt: mgmt, caReader: caReader}
}

// Get implements client.Reader.
func (r *sanitizingKubeconfigReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return errNotAKubeconfigRead
	}
	clusterName, isKubeconfig := strings.CutSuffix(key.Name, "-kubeconfig")
	if !isKubeconfig || clusterName == "" {
		return errNotAKubeconfigRead
	}
	if err := r.mgmt.Get(ctx, key, secret, opts...); err != nil {
		return err
	}
	cluster := &clusterv1.Cluster{}
	if err := r.mgmt.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: clusterName}, cluster); err != nil {
		return fmt.Errorf("get cluster %s/%s for its kubeconfig: %w", key.Namespace, clusterName, err)
	}
	sanitized, err := trustedWorkloadKubeconfig(ctx, r.caReader, cluster, secret.Data[workloadKubeconfigSecretKey])
	if err != nil {
		return err
	}
	// Replace the whole data map: nothing but the rebuilt kubeconfig leaves this
	// reader.
	secret.Data = map[string][]byte{workloadKubeconfigSecretKey: sanitized}
	secret.StringData = nil
	return nil
}

// List implements client.Reader. ClusterCache never lists through its
// SecretClient; fail closed rather than return unsanitized Secrets.
func (r *sanitizingKubeconfigReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errNotAKubeconfigRead
}
