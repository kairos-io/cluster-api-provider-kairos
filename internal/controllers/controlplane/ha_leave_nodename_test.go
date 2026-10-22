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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// withKairosConfigRef points m's bootstrap at the KairosConfig named name, as
// createControlPlaneMachine does.
func withKairosConfigRef(m *clusterv1.Machine, name string) *clusterv1.Machine {
	m.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: bootstrapv1beta2.GroupVersion.Group, Kind: "KairosConfig", Name: name,
	}
	return m
}

// kairosConfigWithHostname is a KairosConfig in "default" with spec.hostname.
func kairosConfigWithHostname(name, hostname string) *bootstrapv1beta2.KairosConfig {
	return &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       bootstrapv1beta2.KairosConfigSpec{Hostname: hostname},
	}
}

// TestLeaveNodeName: the Node name when there is one; otherwise the hostname
// the renderer gives the node, which is spec.hostname over the Machine name,
// lowercased as k0s names the EtcdMember.
func TestLeaveNodeName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine *clusterv1.Machine
		objs    []client.Object
		want    string
	}{
		{
			name:    "the NodeRef wins",
			machine: withKairosConfigRef(nodeMachine("kcp-1", true), "kcp-1"),
			objs:    []client.Object{kairosConfigWithHostname("kcp-1", "other")},
			want:    "kcp-1",
		},
		{
			name:    "no NodeRef: spec.hostname",
			machine: withKairosConfigRef(noNodeRef(nodeMachine("kcp-1", true)), "kcp-1"),
			objs:    []client.Object{kairosConfigWithHostname("kcp-1", "CP-Host")},
			want:    "cp-host",
		},
		{
			name:    "no NodeRef, no spec.hostname: the Machine name",
			machine: withKairosConfigRef(noNodeRef(nodeMachine("kcp-1", true)), "kcp-1"),
			objs:    []client.Object{kairosConfigWithHostname("kcp-1", "")},
			want:    "kcp-1",
		},
		{
			name:    "no NodeRef, the KairosConfig is gone: the Machine name",
			machine: withKairosConfigRef(noNodeRef(nodeMachine("kcp-1", true)), "kcp-1"),
			want:    "kcp-1",
		},
		{
			name:    "no NodeRef, no KairosConfig reference: the Machine name",
			machine: noNodeRef(nodeMachine("kcp-1", true)),
			want:    "kcp-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			r := &KairosControlPlaneReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build(), Scheme: scheme}
			got, err := r.leaveNodeName(context.Background(), tc.machine)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestLeaveNodeName_ReadErrorIsNotAGuess: a KairosConfig that cannot be read
// returns an error, never the Machine name, which may be the wrong member.
func TestLeaveNodeName_ReadErrorIsNotAGuess(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kairosConfigWithHostname("kcp-1", "cp-host")).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok {
				return errors.New("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	_, err := r.leaveNodeName(context.Background(), withKairosConfigRef(noNodeRef(nodeMachine("kcp-1", true)), "kcp-1"))
	g.Expect(err).To(HaveOccurred())
}
