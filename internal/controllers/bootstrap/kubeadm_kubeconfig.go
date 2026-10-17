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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file holds the kubeadm worker path's workload-kubeconfig trust helpers.

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
