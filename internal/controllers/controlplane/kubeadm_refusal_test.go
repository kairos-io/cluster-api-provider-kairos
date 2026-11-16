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

package controlplane

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestReconcile_RefusesKubeadm is the kubeadm refusal (ADR 0010 P1 item 2),
// made unconditional by ADR 0016. A KairosControlPlane whose
// KairosConfigTemplate is kubeadm is refused whatever spec.distribution says;
// before ADR 0016 the check ran only when spec.distribution was empty, so an
// explicit k0s or k3s built control-plane Machines from a kubeadm worker spec.
// An explicit kubeadm that bypassed the webhook is refused too.
//
// With no Machines the refusal surface is today's: Ready and Available
// False(Warning, UnsupportedDistribution) and the failure fields. With
// Machines it holds like a mismatch: Ready and MachinesUpToDate, never
// Available, and no failure fields. Either way nothing is created or deleted,
// kubeadm is never written into spec.distribution (the webhook would reject it
// on every pass), and the KCP comes back every 60s, since no
// KairosConfigTemplate watch would notice a fix.
func TestReconcile_RefusesKubeadm(t *testing.T) {
	for _, tc := range []struct {
		name         string
		spec         string
		templateDist string
		machines     []string
	}{
		{name: "empty spec, kubeadm template", spec: "", templateDist: "kubeadm"},
		{name: "explicit k0s, kubeadm template", spec: "k0s", templateDist: "kubeadm"},
		{name: "explicit k3s, kubeadm template", spec: "k3s", templateDist: "kubeadm"},
		{name: "explicit kubeadm past the webhook", spec: "kubeadm", templateDist: "k0s"},
		{name: "explicit k0s, kubeadm template, Machines exist", spec: "k0s", templateDist: "kubeadm", machines: []string{"k0s"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := newKCPTestScheme(t)

			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
				Spec: clusterv1.ClusterSpec{ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: controlplanev1beta2.GroupVersion.Group, Kind: "KairosControlPlane", Name: "test-kcp",
				}},
			}
			kcp := guardKCP(tc.spec, 1)
			objs := append([]client.Object{cluster, kcp, newDistTemplate("cfg-template", "default", tc.templateDist)},
				guardObjects(kcp, tc.machines...)...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithStatusSubresource(&controlplanev1beta2.KairosControlPlane{}, &clusterv1.Cluster{}).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kcp)})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(distributionHoldRequeueAfter))

			got := &controlplanev1beta2.KairosControlPlane{}
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), got)).To(Succeed())
			g.Expect(got.Spec.Distribution).To(Equal(tc.spec), "spec.distribution is never written while kubeadm is involved")

			machines := &clusterv1.MachineList{}
			g.Expect(c.List(context.Background(), machines, client.InNamespace("default"))).To(Succeed())
			g.Expect(machines.Items).To(HaveLen(len(tc.machines)), "nothing is created or deleted")
			configs := &bootstrapv1beta2.KairosConfigList{}
			g.Expect(c.List(context.Background(), configs, client.InNamespace("default"))).To(Succeed())
			g.Expect(configs.Items).To(HaveLen(len(tc.machines)), "no control-plane KairosConfig is stamped")

			ready := conditions.Get(got, clusterv1.ReadyCondition)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(corev1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
			g.Expect(ready.Severity).To(Equal(clusterv1.ConditionSeverityWarning))
			g.Expect(ready.Message).To(ContainSubstring("kubeadm is not supported on a KairosControlPlane"))

			if len(tc.machines) == 0 {
				avail := conditions.Get(got, controlplanev1beta2.AvailableCondition)
				g.Expect(avail).NotTo(BeNil())
				g.Expect(avail.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(avail.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
				g.Expect(avail.Severity).To(Equal(clusterv1.ConditionSeverityWarning))
				g.Expect(got.Status.FailureReason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
				g.Expect(got.Status.FailureMessage).To(ContainSubstring("kubeadm is not supported on a KairosControlPlane"))
				return
			}
			upToDate := conditions.Get(got, controlplanev1beta2.MachinesUpToDateCondition)
			g.Expect(upToDate).NotTo(BeNil())
			g.Expect(upToDate.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
			g.Expect(conditions.GetReason(got, controlplanev1beta2.AvailableCondition)).NotTo(Equal(controlplanev1beta2.UnsupportedDistributionReason),
				"a control plane with Machines is not reported unavailable for this")
			g.Expect(got.Status.FailureReason).To(BeEmpty())
			g.Expect(got.Status.FailureMessage).To(BeEmpty())
		})
	}
}

// TestResolveEffectiveDistribution_Kubeadm proves the template read reports a
// kubeadm template verbatim, so reconcileDistribution refuses it (ADR 0016
// verdict 1) instead of writing it into spec.distribution.
func TestResolveEffectiveDistribution_Kubeadm(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(newDistTemplate("kubeadm-template", "default", kcpUnsupportedDistributionKubeadm)).
		Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	got, err := r.resolveEffectiveDistribution(context.Background(), newKCPWithTemplate("", "kubeadm-template"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(kcpUnsupportedDistributionKubeadm))
}
