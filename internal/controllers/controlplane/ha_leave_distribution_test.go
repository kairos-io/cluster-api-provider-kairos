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
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// kairosConfigFor returns the KairosConfig m's configRef names, recording
// distribution as createControlPlaneMachine stamps it.
func kairosConfigFor(m *clusterv1.Machine, distribution string) *bootstrapv1beta2.KairosConfig {
	return &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: m.Spec.Bootstrap.ConfigRef.Name, Namespace: m.Namespace},
		Spec:       bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: distribution},
	}
}

// failKairosConfigGets makes every KairosConfig read fail.
func failKairosConfigGets() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok {
				return errors.New("injected KairosConfig read failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// TestMachineDistribution covers the per-Machine read ADR 0016 rests on: the
// KairosConfig's distribution, with an empty value read as k0s, and no vote
// when the Machine names no KairosConfig or it is gone. Other read errors are
// returned.
func TestMachineDistribution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ref       bool
		config    string // "" with withConfig = KairosConfig with no distribution
		withCfg   bool
		failGets  bool
		wantDist  string
		wantFound bool
		wantErr   bool
	}{
		{name: "k3s config", ref: true, withCfg: true, config: "k3s", wantDist: "k3s", wantFound: true},
		{name: "k0s config", ref: true, withCfg: true, config: "k0s", wantDist: "k0s", wantFound: true},
		{name: "empty distribution reads as k0s", ref: true, withCfg: true, config: "", wantDist: "k0s", wantFound: true},
		{name: "KairosConfig not found", ref: true},
		{name: "no configRef", ref: false},
		{name: "read error", ref: true, withCfg: true, config: "k0s", failGets: true, wantErr: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "default"}}
			if tc.ref {
				withKairosConfigRef(m, m.Name)
			}
			b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m)
			if tc.withCfg {
				b = b.WithObjects(kairosConfigFor(m, tc.config))
			}
			if tc.failGets {
				b = b.WithInterceptorFuncs(failKairosConfigGets())
			}
			r := &KairosControlPlaneReconciler{Client: b.Build(), Scheme: scheme}
			dist, found, err := r.machineDistribution(context.Background(), m)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(found).To(BeFalse())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(Equal(tc.wantFound))
			g.Expect(dist).To(Equal(tc.wantDist))
		})
	}
}

// TestReconcileMemberLeave_TheHookDecides is ADR 0016 D4 as ruled after
// review: the etcd-leave hook decides, never the KairosConfig and never
// spec.distribution. A hooked Machine whose KairosConfig says k3s is not
// released without a leave: it waits for CAPI like any k0s member, then gets
// the leave request (or the bounded unreachable phase), and a Warning event
// says the KairosConfig contradicts the hook. A KairosConfig that cannot be
// read is only logged.
func TestReconcileMemberLeave_TheHookDecides(t *testing.T) {
	for _, tc := range []struct {
		name        string
		specDist    string
		configDist  string // "" = no KairosConfig object
		ref         bool
		failGets    bool
		preTerm     bool // CAPI is at the pre-terminate hook
		answers     bool // a verified workload apiserver answers
		wantErr     bool
		wantRequest bool // the leave-requested sentinel is written
		wantEvent   bool
	}{
		{name: "k3s config, CAPI still draining: waits, no event", specDist: "k0s", ref: true, configDist: "k3s"},
		{name: "k3s config at pre-terminate: leave requested, Warning", specDist: "k0s", ref: true, configDist: "k3s",
			preTerm: true, answers: true, wantRequest: true, wantEvent: true},
		{name: "k3s config at pre-terminate, nothing answers: unreachable phase, Warning", specDist: "k0s", ref: true, configDist: "k3s",
			preTerm: true, wantErr: true, wantEvent: true},
		{name: "k0s config under a k3s spec: leave requested, no event", specDist: "k3s", ref: true, configDist: "k0s",
			preTerm: true, answers: true, wantRequest: true},
		{name: "KairosConfig gone: leave requested, no event", specDist: "k3s", ref: true,
			preTerm: true, answers: true, wantRequest: true},
		{name: "no configRef: leave requested, no event", specDist: "k3s",
			preTerm: true, answers: true, wantRequest: true},
		{name: "KairosConfig read error: logged, leave requested", specDist: "k0s", ref: true, configDist: "k3s", failGets: true,
			preTerm: true, answers: true, wantRequest: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			target := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
				Name: "cp-0", Namespace: "default",
				Annotations: map[string]string{etcdLeaveHookAnnotation(): ""},
			}}
			target.Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp-0"}
			if tc.preTerm {
				atPreTerminateHook(target)
			}
			if tc.ref {
				withKairosConfigRef(target, target.Name)
			}
			b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target)
			if tc.configDist != "" {
				b = b.WithObjects(kairosConfigFor(target, tc.configDist))
			}
			if tc.failGets {
				b = b.WithInterceptorFuncs(failKairosConfigGets())
			}
			c := b.Build()
			wc := pinnedWorkloadBuilder(scheme).Build()
			rec := record.NewFakeRecorder(4)
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: rec}
			r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
				if !tc.answers {
					return nil, errors.New("the control-plane endpoint does not answer")
				}
				return wc, nil
			}
			kcp := pinnedK0sKCP()
			kcp.Spec.Distribution = tc.specDist

			done, err := r.reconcileMemberLeave(context.Background(), log.Log, kcp, testCluster(), []*clusterv1.Machine{target}, target)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
			g.Expect(done).To(BeFalse(), "a hooked Machine is never released without a leave")

			got := &clusterv1.Machine{}
			g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "cp-0", Namespace: "default"}, got)).To(Succeed())
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())

			cm := &corev1.ConfigMap{}
			cmErr := wc.Get(context.Background(), types.NamespacedName{Name: etcdLeaveConfigMapName, Namespace: etcdLeaveConfigMapNamespace}, cm)
			if tc.wantRequest {
				g.Expect(cmErr).ToNot(HaveOccurred())
				g.Expect(cm.Data).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
				g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
			} else {
				g.Expect(apierrors.IsNotFound(cmErr)).To(BeTrue(), "no leave request is written")
			}
			if tc.wantEvent {
				g.Expect(rec.Events).To(Receive(SatisfyAll(
					ContainSubstring(etcdLeaveHookContradictedEventReason),
					ContainSubstring(`"cp-0" says k3s`))))
			}
			g.Expect(rec.Events).NotTo(Receive())
		})
	}
}

// TestReconcileMemberLeave_ContradictionWarnedOncePerCycle: the Warning is
// emitted on the first pass that acts, and not again on the passes that
// follow, whether the leave was requested or the unreachable phase began.
func TestReconcileMemberLeave_ContradictionWarnedOncePerCycle(t *testing.T) {
	for _, answers := range []bool{true, false} {
		answers := answers
		t.Run(fmt.Sprintf("workload answers=%t", answers), func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			target := withKairosConfigRef(hookedMachine("cp-0", "cp-0", nil), "cp-0")
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(target, kairosConfigFor(target, "k3s"), etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
			wc := pinnedWorkloadBuilder(scheme).Build()
			rec := record.NewFakeRecorder(8)
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: rec,
				WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
					if !answers {
						return nil, errors.New("the control-plane endpoint does not answer")
					}
					return wc, nil
				}}
			for range 3 {
				_, _ = r.reconcileMemberLeave(context.Background(), log.Log, pinnedK0sKCP(), testCluster(), []*clusterv1.Machine{target}, target)
				g.Expect(hasEtcdLeaveHook(target)).To(BeTrue())
			}
			g.Expect(rec.Events).To(Receive(ContainSubstring(etcdLeaveHookContradictedEventReason)))
			g.Expect(rec.Events).NotTo(Receive(), "the contradiction is reported once per eviction cycle")
		})
	}
}
