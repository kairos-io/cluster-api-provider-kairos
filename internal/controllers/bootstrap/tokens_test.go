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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

func tokenTestScheme(g *WithT) *runtime.Scheme {
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func tokenSecret(name, ns, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func tokenReconciler(g *WithT, objs ...client.Object) *KairosConfigReconciler {
	scheme := tokenTestScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &KairosConfigReconciler{Client: c, Scheme: scheme}
}

// TestResolveToken_K0sWorkerPrecedence asserts the k0s worker precedence chain
// (WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token) resolves to the
// highest-precedence source present, against observed output (the returned
// token), not internal state.
func TestResolveToken_K0sWorkerPrecedence(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	tests := []struct {
		name  string
		spec  bootstrapv1beta2.KairosConfigSpec
		objs  []client.Object
		want  string
		errIs error
	}{
		{
			name: "WorkerTokenSecretRef wins over all inline",
			spec: bootstrapv1beta2.KairosConfigSpec{
				WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt"},
				WorkerToken:          "inline-worker",
				Token:                "legacy-inline",
			},
			objs: []client.Object{tokenSecret("wt", "default", "token", "from-secret")},
			want: "from-secret",
		},
		{
			name: "WorkerToken inline when no ref",
			spec: bootstrapv1beta2.KairosConfigSpec{WorkerToken: "inline-worker", Token: "legacy"},
			want: "inline-worker",
		},
		{
			name: "legacy TokenSecretRef when no worker fields",
			spec: bootstrapv1beta2.KairosConfigSpec{
				TokenSecretRef: &corev1.ObjectReference{Name: "legacy-secret"},
				Token:          "legacy-inline",
			},
			objs: []client.Object{tokenSecret("legacy-secret", "default", "value", "from-legacy")},
			want: "from-legacy",
		},
		{
			name: "legacy inline Token last",
			spec: bootstrapv1beta2.KairosConfigSpec{Token: "legacy-inline"},
			want: "legacy-inline",
		},
		{
			name:  "missing referenced secret requeues (errTokenNotReady)",
			spec:  bootstrapv1beta2.KairosConfigSpec{WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "absent"}},
			errIs: errTokenNotReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
				Spec:       tc.spec,
			}
			r := tokenReconciler(g, tc.objs...)
			got, err := r.resolveToken(context.Background(), tokenKindK0sWorker, kc, cluster)
			if tc.errIs != nil {
				g.Expect(err).To(MatchError(tc.errIs))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestResolveToken_K3sWorkerPrecedence asserts the k3s worker chain prefers the
// k3s-specific fields ahead of the shared worker/legacy tail.
func TestResolveToken_K3sWorkerPrecedence(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	tests := []struct {
		name  string
		spec  bootstrapv1beta2.KairosConfigSpec
		objs  []client.Object
		want  string
		errIs error
	}{
		{
			name: "K3sTokenSecretRef wins",
			spec: bootstrapv1beta2.KairosConfigSpec{
				K3sTokenSecretRef:    &bootstrapv1beta2.WorkerTokenSecretReference{Name: "k3s"},
				K3sToken:             "inline-k3s",
				WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt"},
			},
			objs: []client.Object{
				tokenSecret("k3s", "default", "token", "from-k3s-secret"),
				tokenSecret("wt", "default", "token", "from-worker-secret"),
			},
			want: "from-k3s-secret",
		},
		{
			name: "K3sToken inline ahead of worker fields",
			spec: bootstrapv1beta2.KairosConfigSpec{K3sToken: "inline-k3s", WorkerToken: "inline-worker"},
			want: "inline-k3s",
		},
		{
			name: "falls through to worker tail",
			spec: bootstrapv1beta2.KairosConfigSpec{WorkerToken: "inline-worker"},
			want: "inline-worker",
		},
		{
			name:  "missing k3s secret requeues",
			spec:  bootstrapv1beta2.KairosConfigSpec{K3sTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "absent"}},
			errIs: errTokenNotReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
				Spec:       tc.spec,
			}
			r := tokenReconciler(g, tc.objs...)
			got, err := r.resolveToken(context.Background(), tokenKindK3sWorker, kc, cluster)
			if tc.errIs != nil {
				g.Expect(err).To(MatchError(tc.errIs))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestResolveToken_ControlPlaneJoin asserts the Phase-3 control-plane-join arm:
// k3s resolves from K3sTokenSecretRef, k0s from ControlPlaneJoinTokenSecretRef,
// and a missing/absent ref requeues (errTokenNotReady) — never an inline source
// (TOKEN-INV).
func TestResolveToken_ControlPlaneJoin(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	tests := []struct {
		name  string
		spec  bootstrapv1beta2.KairosConfigSpec
		objs  []client.Object
		want  string
		errIs error
	}{
		{
			name: "k3s resolves shared server token from K3sTokenSecretRef",
			spec: bootstrapv1beta2.KairosConfigSpec{
				Distribution:      "k3s",
				K3sTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "cluster-join"},
			},
			objs: []client.Object{tokenSecret("cluster-join", "default", "token", "shared-server-token")},
			want: "shared-server-token",
		},
		{
			name: "k0s resolves controller-join token from ControlPlaneJoinTokenSecretRef",
			spec: bootstrapv1beta2.KairosConfigSpec{
				Distribution:                   "k0s",
				ControlPlaneJoinTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "cp-join"},
			},
			objs: []client.Object{tokenSecret("cp-join", "default", "token", "k0s-controller-token")},
			want: "k0s-controller-token",
		},
		{
			name:  "k0s with no ref requeues",
			spec:  bootstrapv1beta2.KairosConfigSpec{Distribution: "k0s"},
			errIs: errTokenNotReady,
		},
		{
			name:  "k3s with absent secret requeues",
			spec:  bootstrapv1beta2.KairosConfigSpec{Distribution: "k3s", K3sTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "absent"}},
			errIs: errTokenNotReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
				Spec:       tc.spec,
			}
			r := tokenReconciler(g, tc.objs...)
			got, err := r.resolveToken(context.Background(), tokenKindControlPlaneJoin, kc, cluster)
			if tc.errIs != nil {
				g.Expect(err).To(MatchError(tc.errIs))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestResolveToken_KeylessSecretIsHardError asserts that a present Secret that
// lacks the expected data key is a hard error (not a requeue) — the operator
// mis-keyed the Secret and retrying will not fix it.
func TestResolveToken_KeylessSecretIsHardError(t *testing.T) {
	g := NewWithT(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt", Key: "token"},
		},
	}
	r := tokenReconciler(g, tokenSecret("wt", "default", "wrong-key", "x"))
	_, err := r.resolveToken(context.Background(), tokenKindK0sWorker, kc, cluster)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err).ToNot(MatchError(errTokenNotReady))
}
