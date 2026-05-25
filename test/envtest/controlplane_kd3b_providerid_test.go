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

package envtest

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestProviderID_NotPatchedByController is the PR-8 regression guard
// (KD-3b): once the controller-side patch was deleted, Node.Spec.ProviderID
// is the in-VM cloud-config's responsibility alone (kubelet --provider-id
// for k3s, systemd ExecStartPre drop-in, post-bootstrap `kubectl patch`
// fallback). This test pins that responsibility model so a future refactor
// can't accidentally reintroduce a controller-side Node patch.
//
// The test:
//
//  1. Brings up a fresh envtest + KCP reconciler.
//  2. Creates a Cluster, KairosControlPlane, control-plane Machine, and a
//     fake "workload" Node with empty Spec.ProviderID and a matching
//     address. The Node is created IN THE ENVTEST API SERVER — i.e. the
//     same cluster the controller is reconciling against — which mimics
//     what the deleted ensureProviderIDOnNodes would have reached via the
//     workload-cluster client.
//  3. Writes the workload kubeconfig Secret with a credentials payload
//     that ACTUALLY POINTS to the envtest API server (constructed from
//     testEnv's *rest.Config). This makes the regression guard
//     consequential: if someone resurrects a controller-side patch, the
//     resurrected code will follow the kubeconfig back into the same
//     envtest API and try to mutate the Node — and the assertion will
//     fire.
//  4. Waits for the controller to reach KubeconfigReady=True (proving the
//     KCP Reconcile chain actually executed end-to-end and would have hit
//     any node-patching code in that chain).
//  5. Asserts Consistently for 10s that the Node's Spec.ProviderID stays
//     empty — i.e. the controller does NOT write it.
//
// The assertion is Consistently, not Eventually-fails: we want to catch a
// regression that EVENTUALLY patches, not just a regression that patches
// in the first reconcile.
func TestProviderID_NotPatchedByController(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, cfg, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kd3b-providerid-ns"},
	}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "kd3b-providerid-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				Namespace:  ns.Name,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kcp",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "missing-on-purpose",
					Namespace:  ns.Name,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "missing-on-purpose",
			},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Pre-create a control-plane Machine so reconcileMachines doesn't try to
	// invoke createControlPlaneMachine (which would fail without the infra/
	// config templates that don't exist in envtest). Same pattern as the
	// PR-7 node-push test.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: ns.Name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: controlplanev1beta2.GroupVersion.String(),
					Kind:       "KairosControlPlane",
					Name:       kcp.Name,
					UID:        kcp.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			Version:     ptr.To("v1.30.0+k0s.0"),
			// ProviderID intentionally left nil so it can't trivially
			// satisfy a regression that only ever wrote when ProviderID
			// was already known.
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// Create a fake workload Node in the envtest API server. Empty
	// ProviderID is the precondition; the test asserts it stays empty.
	// The Node address matches the Machine's name so a regression that
	// matches by hostname (rather than by address) is still caught.
	nodeName := machine.Name
	workloadNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Spec: corev1.NodeSpec{
			// ProviderID: "" — INVARIANT under test.
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.99.99.1"},
				{Type: corev1.NodeHostName, Address: nodeName},
			},
		},
	}
	g.Expect(c.Create(ctx, workloadNode)).To(Succeed())

	// Construct a kubeconfig that actually points to the envtest API
	// server, so that if any resurrected controller-side patch code uses
	// the kubeconfig, the patch lands on the workloadNode above.
	kubeconfigBytes, err := buildEnvtestKubeconfig(cfg)
	g.Expect(err).NotTo(HaveOccurred())

	pushedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			Annotations: map[string]string{
				"controllers.cluster.x-k8s.io/kubeconfig-source": "node-push",
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": kubeconfigBytes},
	}
	g.Expect(c.Create(ctx, pushedSecret)).To(Succeed())

	// Wait for KCP to observe the Secret and reach KubeconfigReady=True.
	// This is the proxy signal that the controller's Reconcile chain has
	// executed end-to-end at least once with the Secret in place. Any
	// resurrected ensureProviderIDOnNodes-style step would have run by
	// this point.
	g.Eventually(func() corev1.ConditionStatus {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return ""
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, 30*time.Second, time.Second).Should(Equal(corev1.ConditionTrue))

	// The actual regression assertion. Consistently (not Eventually) — a
	// regression that patches ProviderID on the SECOND reconcile would be
	// just as bad as one that patches on the first.
	g.Consistently(func() string {
		got := &corev1.Node{}
		if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, got); err != nil {
			return "ERR: " + err.Error()
		}
		return got.Spec.ProviderID
	}, 10*time.Second, 500*time.Millisecond).Should(BeEmpty(),
		"controller must not patch Node.Spec.ProviderID — the in-VM cloud-config owns that field (KD-3b PR-8)")
}

// buildEnvtestKubeconfig renders a kubeconfig YAML pointing at the envtest
// apiserver. Used by the PR-8 regression test so that if anyone resurrects
// a controller-side workload-cluster patch path, the kubeconfig actually
// connects somewhere mutate-able and the test will catch the regression.
//
// envtest exposes the apiserver with cert-based auth: cfg.CAData /
// cfg.CertData / cfg.KeyData. We render all three into the standard
// kubeconfig schema; clientcmd.Write handles base64 encoding.
func buildEnvtestKubeconfig(cfg *rest.Config) ([]byte, error) {
	const ctxName = "envtest"
	api := clientcmdapi.NewConfig()
	api.Clusters[ctxName] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	api.AuthInfos[ctxName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	api.Contexts[ctxName] = &clientcmdapi.Context{
		Cluster:  ctxName,
		AuthInfo: ctxName,
	}
	api.CurrentContext = ctxName
	return clientcmd.Write(*api)
}
