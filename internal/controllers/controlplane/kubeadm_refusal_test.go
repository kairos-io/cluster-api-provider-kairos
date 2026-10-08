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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestRefuseKubeadmControlPlane asserts the P1 refusal path (ADR 0010 item 2):
// a kubeadm KairosControlPlane is refused with a clear Available/Ready
// False(Warning, UnsupportedDistribution) condition and latched failure fields,
// and the refusal returns without a requeue and without ever writing kubeadm into
// spec.distribution (which would loop against the webhook).
func TestRefuseKubeadmControlPlane(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	kcp := newKCPWithTemplate("", "kubeadm-template")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kcp).
		WithStatusSubresource(&controlplanev1beta2.KairosControlPlane{}).
		Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	res, err := r.refuseKubeadmControlPlane(context.Background(), log.Log, kcp,
		`KairosConfigTemplate "kubeadm-template" has distribution kubeadm`)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeFalse())
	g.Expect(res.RequeueAfter).To(BeZero())

	// spec.distribution is never set to kubeadm (no error loop with the webhook).
	g.Expect(kcp.Spec.Distribution).To(BeEmpty())

	// Conditions + failure fields surface the refusal.
	g.Expect(kcp.Status.FailureReason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
	g.Expect(kcp.Status.FailureMessage).To(ContainSubstring("kubeadm is not supported on a KairosControlPlane"))

	avail := conditions.Get(kcp, controlplanev1beta2.AvailableCondition)
	g.Expect(avail).NotTo(BeNil())
	g.Expect(string(avail.Status)).To(Equal("False"))
	g.Expect(avail.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
	g.Expect(avail.Severity).To(Equal(clusterv1.ConditionSeverityWarning))

	ready := conditions.Get(kcp, clusterv1.ReadyCondition)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
}

// TestResolveEffectiveDistribution_Kubeadm proves the inherit source reports a
// kubeadm template verbatim, so the Reconcile guard (kcpUnsupportedDistributionKubeadm)
// fires before the persist-and-loop path.
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
