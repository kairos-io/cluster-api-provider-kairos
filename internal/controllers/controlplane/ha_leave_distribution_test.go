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
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// TestReconcileMemberLeave_DecidesFromTheMachinesConfig is ADR 0016 D4: step
// (1) removes the hook without a leave only when the leaving Machine's own
// KairosConfig says it is not k0s. spec.distribution does not decide it, so a
// k0s member of a control plane relabelled k3s keeps its hook and goes through
// the leave. A KairosConfig that is gone, or no configRef at all, leaves the
// hook to speak, and only k0s HA Machines are given one. A failed read keeps
// the hook.
//
// The Machine is not yet at CAPI's pre-terminate stage, so a Machine that gets
// past step (1) stops at step (2b) without any workload traffic: done=false
// and the hook kept is exactly "not released by step (1)".
func TestReconcileMemberLeave_DecidesFromTheMachinesConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		specDist   string
		ref        bool
		configDist string // "" = no KairosConfig object
		failGets   bool
		wantDone   bool
		wantHook   bool
		wantErr    bool
	}{
		{name: "k3s config under a k0s spec: hook removed", specDist: "k0s", ref: true, configDist: "k3s", wantDone: true, wantHook: false},
		{name: "k0s config under a k3s spec: hook kept, leave proceeds", specDist: "k3s", ref: true, configDist: "k0s", wantHook: true},
		{name: "k0s config under a k0s spec: hook kept", specDist: "k0s", ref: true, configDist: "k0s", wantHook: true},
		{name: "KairosConfig gone under a k3s spec: treated as k0s", specDist: "k3s", ref: true, wantHook: true},
		{name: "no configRef under a k3s spec: treated as k0s", specDist: "k3s", ref: false, wantHook: true},
		{name: "read error keeps the hook", specDist: "k3s", ref: true, configDist: "k3s", failGets: true, wantHook: true, wantErr: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			// Hooked, with a NodeRef, but CAPI is still draining: no Deleting
			// condition at WaitingForPreTerminateHook.
			target := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
				Name: "cp-0", Namespace: "default",
				Annotations: map[string]string{etcdLeaveHookAnnotation(): ""},
			}}
			target.Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp-0"}
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

			workloadCalls := 0
			r := &KairosControlPlaneReconciler{
				Client: c, Scheme: scheme,
				WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
					workloadCalls++
					return nil, errors.New("no workload traffic expected")
				},
			}
			kcp := pinnedK0sKCP()
			kcp.Spec.Distribution = tc.specDist

			done, err := r.reconcileMemberLeave(context.Background(), log.Log, kcp, testCluster(), []*clusterv1.Machine{target}, target)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
			g.Expect(done).To(Equal(tc.wantDone))
			g.Expect(workloadCalls).To(BeZero(), "neither step (1) nor step (2b) talks to the workload cluster")

			got := &clusterv1.Machine{}
			g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "cp-0", Namespace: "default"}, got)).To(Succeed())
			g.Expect(hasEtcdLeaveHook(got)).To(Equal(tc.wantHook))
		})
	}
}
