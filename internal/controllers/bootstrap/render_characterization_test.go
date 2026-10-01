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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// These tests pin behaviour the goldens cannot observe: the exact user-visible
// error strings (copied verbatim into three conditions and status.failureMessage),
// the field-set isolation invariant (I1), the token-before-password ordering, and
// the regeneration/render contracts (R4, S2). They characterise CURRENT behaviour
// before the P0 distribution-seam refactor; no later P0 commit may change them
// except the dedicated OQ-A fix (which updates the k0s LB-error assertion here).

func charScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := bootstrapv1beta2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func charCluster(withEndpoint bool) *clusterv1.Cluster {
	c := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"}}
	if withEndpoint {
		c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "10.0.0.42", Port: 6443}
	}
	return c
}

func charMachine(infraKind, providerID string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			ProviderID:        providerID,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{Kind: infraKind},
		},
	}
}

func workerKC(dist string, mutate func(s *bootstrapv1beta2.KairosConfigSpec)) *bootstrapv1beta2.KairosConfig {
	spec := bootstrapv1beta2.KairosConfigSpec{
		Role:         "worker",
		Distribution: dist,
		UserName:     "kairos",
		UserPassword: "test-password",
		UserGroups:   []string{"admin"},
	}
	if mutate != nil {
		mutate(&spec)
	}
	return &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
		Spec:       spec,
	}
}

// ---- I1: field-set isolation (goldens cannot catch this) -------------------

// TestGenerateCloudConfig_FieldSetIsolation_K3sIgnoresPodCIDR proves the k3s
// generator never copies spec.podCIDR into TemplateData: a control character in
// podCIDR would trip validateTemplateData's podCIDR check and fail the render if
// it reached TemplateData. The render MUST succeed.
func TestGenerateCloudConfig_FieldSetIsolation_K3sIgnoresPodCIDR(t *testing.T) {
	g := NewWithT(t)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	kc := workerKC("k3s", func(s *bootstrapv1beta2.KairosConfigSpec) {
		s.K3sToken = "k3s-token"
		s.PodCIDR = "10.0.0.0/16\n" // control char: harmless only if k3s drops it
	})
	_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), charCluster(true))
	g.Expect(err).NotTo(HaveOccurred(), "k3s must not route spec.podCIDR into the validated TemplateData.PodCIDR")
}

// TestGenerateCloudConfig_FieldSetIsolation_K0sIgnoresServerAddress proves the
// k0s generator never copies serverAddress into TemplateData.K3sServerURL: a
// control character in serverAddress would trip the k3sServerURL check if it
// reached TemplateData. The render MUST succeed.
func TestGenerateCloudConfig_FieldSetIsolation_K0sIgnoresServerAddress(t *testing.T) {
	g := NewWithT(t)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:          "control-plane",
			Distribution:  "k0s",
			SingleNode:    true,
			UserName:      "kairos",
			UserPassword:  "test-password",
			UserGroups:    []string{"admin"},
			ServerAddress: "https://evil\n.example.com:6443", // control char
		},
	}
	_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", "vsphere://x"), charCluster(false))
	g.Expect(err).NotTo(HaveOccurred(), "k0s must not route serverAddress into the validated TemplateData.K3sServerURL")
}

// ---- M3b: exact error strings on every worker token path -------------------

func TestGenerateCloudConfig_WorkerTokenErrorStrings(t *testing.T) {
	cases := []struct {
		name    string
		dist    string
		mutate  func(s *bootstrapv1beta2.KairosConfigSpec)
		objects []client.Object
		cluster *clusterv1.Cluster
		wantErr string
	}{
		{
			name: "k0s cross-namespace worker token ref (worker token label)",
			dist: "k0s",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.WorkerTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt", Namespace: "other-ns"}
			},
			cluster: charCluster(true),
			wantErr: "worker token: cross-namespace Secret references are not allowed: Secret other-ns/wt is outside namespace default",
		},
		{
			name: "k3s cross-namespace k3s token ref (k3s token label)",
			dist: "k3s",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.K3sTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt", Namespace: "other-ns"}
			},
			cluster: charCluster(true),
			wantErr: "k3s token: cross-namespace Secret references are not allowed: Secret other-ns/wt is outside namespace default",
		},
		{
			name: "worker token ref secret missing key",
			dist: "k0s",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.WorkerTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt"}
			},
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "wt", Namespace: "default"},
				Data:       map[string][]byte{"wrong": []byte("x")},
			}},
			cluster: charCluster(true),
			wantErr: "worker token secret default/wt does not contain key 'token'",
		},
		{
			name: "legacy token ref secret keyless",
			dist: "k0s",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.TokenSecretRef = &corev1.ObjectReference{Name: "legacy"}
			},
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default"},
				Data:       map[string][]byte{"nope": []byte("x")},
			}},
			cluster: charCluster(true),
			wantErr: "token secret does not contain 'token' or 'value' key",
		},
		{
			name:    "k0s worker with no token at all",
			dist:    "k0s",
			mutate:  func(_ *bootstrapv1beta2.KairosConfigSpec) {},
			cluster: charCluster(true),
			wantErr: "worker token is required for worker nodes: either WorkerTokenSecretRef, WorkerToken, TokenSecretRef, or Token must be set",
		},
		{
			name:    "k3s worker with no token at all",
			dist:    "k3s",
			mutate:  func(_ *bootstrapv1beta2.KairosConfigSpec) {},
			cluster: charCluster(true),
			wantErr: "k3s worker requires a join token: set k3sTokenSecretRef, k3sToken, workerTokenSecretRef, workerToken, tokenSecretRef, or token",
		},
		{
			name: "k3s worker with token but no serverAddress",
			dist: "k3s",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.K3sToken = "k3s-token"
			},
			cluster: charCluster(false),
			wantErr: "k3s worker requires serverAddress or cluster controlPlaneEndpoint",
		},
		{
			name:    "unsupported distribution",
			dist:    "foo",
			mutate:  func(_ *bootstrapv1beta2.KairosConfigSpec) {},
			cluster: charCluster(true),
			wantErr: "unsupported distribution: foo",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}

			kc := workerKC(tc.dist, tc.mutate)
			_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), tc.cluster)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(Equal(tc.wantErr))
		})
	}
}

// TestGenerateCloudConfig_TokenGetErrorIsHard pins the wrapped hard-error strings
// for a non-NotFound Secret Get on the worker ref and the legacy ref.
func TestGenerateCloudConfig_TokenGetErrorIsHard(t *testing.T) {
	boom := errors.New("boom")
	getErr := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	cases := []struct {
		name    string
		mutate  func(s *bootstrapv1beta2.KairosConfigSpec)
		wantErr string
	}{
		{
			name: "worker token ref get fails",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.WorkerTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "wt"}
			},
			wantErr: "failed to get worker token secret default/wt: boom",
		},
		{
			name: "legacy token ref get fails",
			mutate: func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.TokenSecretRef = &corev1.ObjectReference{Name: "legacy"}
			},
			wantErr: "failed to get token secret: boom",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(getErr).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}
			kc := workerKC("k0s", tc.mutate)
			_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), charCluster(true))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(Equal(tc.wantErr))
		})
	}
}

// TestGenerateCloudConfig_MissingTokenSecretRequeues pins the requeue signal: a
// referenced-but-absent worker Secret surfaces as errTokenNotReady, not a hard
// error, on both distributions.
func TestGenerateCloudConfig_MissingTokenSecretRequeues(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		dist := dist
		t.Run(dist, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}
			kc := workerKC(dist, func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.WorkerTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "absent"}
			})
			_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), charCluster(true))
			g.Expect(err).To(MatchError(errTokenNotReady))
		})
	}
}

// TestGenerateCloudConfig_TokenErrorBeatsPasswordError pins the read order:
// worker token resolution runs before password resolution, so with both the
// token Secret and the password Secret missing the token error wins (requeue),
// not the password error (hard failure).
func TestGenerateCloudConfig_TokenErrorBeatsPasswordError(t *testing.T) {
	g := NewWithT(t)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}
	kc := workerKC("k0s", func(s *bootstrapv1beta2.KairosConfigSpec) {
		s.UserPassword = ""
		s.UserPasswordSecretRef = &bootstrapv1beta2.UserPasswordSecretReference{Name: "absent-pw"}
		s.WorkerTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "absent-token"}
	})
	_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), charCluster(true))
	g.Expect(err).To(MatchError(errTokenNotReady), "the worker token resolves before the password; its error must win")
}

// TestGenerateCloudConfig_LBEndpointErrorSymmetry pins the LB-endpoint error on
// a CAPK control plane: when the LB Service Get fails with a non-NotFound error,
// BOTH distributions now wrap it identically (OQ-A fix). Before the fix the k0s
// arm returned the bare error.
func TestGenerateCloudConfig_LBEndpointErrorSymmetry(t *testing.T) {
	boom := errors.New("boom")
	lbGetErr := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "test-cluster-"+controlPlaneLBServiceSuffix {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	cases := []struct {
		dist    string
		wantErr string
	}{
		{"k0s", "failed to get control plane LB endpoint: boom"},
		{"k3s", "failed to get control plane LB endpoint: boom"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.dist, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(lbGetErr).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:         "control-plane",
					Distribution: tc.dist,
					SingleNode:   true,
					UserName:     "kairos",
					UserPassword: "test-password",
					UserGroups:   []string{"admin"},
				},
			}
			_, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("KubevirtMachine", "kubevirt://x"), charCluster(true))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(Equal(tc.wantErr))
		})
	}
}

// ---- S2: render contract (the substrings R4 depends on) --------------------

// TestGenerateCloudConfig_RenderContract pins that every distribution/infra
// variant, rendered with a providerID, contains the distribution's
// post-bootstrap marker and an SSH-enable stage. This is the contract the R4
// regeneration heuristic relies on; if a render stopped emitting either, R4
// would loop forever (KD-9).
func TestGenerateCloudConfig_RenderContract(t *testing.T) {
	cases := []struct {
		name       string
		dist       string
		infraKind  string
		providerID string
		marker     string
	}{
		{"k0s_capv", "k0s", "VSphereMachine", "vsphere://x", "kairos-k0s-post-bootstrap.service"},
		{"k0s_capk", "k0s", "KubevirtMachine", "kubevirt://x", "kairos-k0s-post-bootstrap.service"},
		{"k3s_capv", "k3s", "VSphereMachine", "vsphere://x", "kairos-k3s-post-bootstrap.service"},
		{"k3s_capk", "k3s", "KubevirtMachine", "kubevirt://x", "kairos-k3s-post-bootstrap.service"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			objs := []client.Object{}
			if tc.infraKind == "KubevirtMachine" {
				objs = append(objs, &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-" + controlPlaneLBServiceSuffix, Namespace: "default"},
					Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{{IP: "10.96.0.10"}},
					}},
				})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:         "control-plane",
					Distribution: tc.dist,
					SingleNode:   true,
					UserName:     "kairos",
					UserPassword: "test-password",
					UserGroups:   []string{"admin"},
				},
			}
			out, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine(tc.infraKind, tc.providerID), charCluster(true))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring(tc.providerID), "render must embed the providerID")
			g.Expect(out).To(ContainSubstring(tc.marker), "render must contain the post-bootstrap marker R4 looks for")
			g.Expect(out).To(Or(
				ContainSubstring("systemctl enable --now sshd"),
				ContainSubstring("systemctl enable --now ssh"),
			), "render must contain the SSH-enable stage R4 looks for")
		})
	}
}

// ---- R4: regeneration decision on an existing bootstrap Secret -------------

// TestReconcileBootstrapData_Regeneration pins the substring-based regeneration
// heuristic (R4, KD-9): a Secret that already carries the providerID, the
// post-bootstrap marker AND the SSH-enable stage is left alone; one missing the
// marker or the SSH-enable stage is regenerated. Characterised for both distros.
func TestReconcileBootstrapData_Regeneration(t *testing.T) {
	const providerID = "vsphere://vm-uuid"
	sshStage := "systemctl enable --now sshd"

	cases := []struct {
		name           string
		dist           string
		marker         string
		content        string
		wantRegenerate bool
	}{
		{
			name:           "k0s up-to-date is left alone",
			dist:           "k0s",
			content:        "#cloud-config\n# " + providerID + "\n# kairos-k0s-post-bootstrap.service\n# " + sshStage + "\n",
			wantRegenerate: false,
		},
		{
			name:           "k0s missing marker regenerates",
			dist:           "k0s",
			content:        "#cloud-config\n# " + providerID + "\n# " + sshStage + "\n",
			wantRegenerate: true,
		},
		{
			name:           "k0s missing ssh-enable regenerates",
			dist:           "k0s",
			content:        "#cloud-config\n# " + providerID + "\n# kairos-k0s-post-bootstrap.service\n",
			wantRegenerate: true,
		},
		{
			name:           "k3s up-to-date is left alone",
			dist:           "k3s",
			content:        "#cloud-config\n# " + providerID + "\n# kairos-k3s-post-bootstrap.service\n# " + sshStage + "\n",
			wantRegenerate: false,
		},
		{
			name:           "k3s missing marker regenerates",
			dist:           "k3s",
			content:        "#cloud-config\n# " + providerID + "\n# " + sshStage + "\n",
			wantRegenerate: true,
		},
		{
			name:           "k3s missing ssh-enable regenerates",
			dist:           "k3s",
			content:        "#cloud-config\n# " + providerID + "\n# kairos-k3s-post-bootstrap.service\n",
			wantRegenerate: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			const secretName = "test-config"
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: "default",
					Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
				},
				Type: clusterv1.ClusterSecretType,
				Data: map[string][]byte{"value": []byte(tc.content)},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}

			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default", UID: "kc-uid"},
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:         "control-plane",
					Distribution: tc.dist,
					SingleNode:   true,
					UserName:     "kairos",
					UserPassword: "test-password",
					UserGroups:   []string{"admin"},
				},
				Status: bootstrapv1beta2.KairosConfigStatus{DataSecretName: ptr.To(secretName)},
			}
			machine := charMachine("VSphereMachine", providerID)
			cluster := charCluster(true)

			_, err := r.reconcileBootstrapData(context.Background(), log.Log, kc, machine, cluster)
			g.Expect(err).NotTo(HaveOccurred())

			got := &corev1.Secret{}
			g.Expect(c.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: "default"}, got)).To(Succeed())
			changed := string(got.Data["value"]) != tc.content
			g.Expect(changed).To(Equal(tc.wantRegenerate),
				"regenerate decision mismatch: content changed=%v, want regenerate=%v", changed, tc.wantRegenerate)
		})
	}
}

// ---- Manifests field coverage (held out of the YAML goldens) ---------------

// TestGenerateCloudConfig_ManifestsCopied characterises that both generators
// copy spec.Manifests into the render (the manifest write stage), identically.
// It does not assert YAML validity: the manifest heredoc renders its content at
// column 0 inside a content:| block scalar, a pre-existing template trait out of
// P0 scope (see render_golden_test.go).
func TestGenerateCloudConfig_ManifestsCopied(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		dist := dist
		t.Run(dist, func(t *testing.T) {
			g := NewWithT(t)
			scheme := charScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme}
			kc := &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:         "control-plane",
					Distribution: dist,
					SingleNode:   true,
					UserName:     "kairos",
					UserPassword: "test-password",
					UserGroups:   []string{"admin"},
					Manifests: []bootstrapv1beta2.Manifest{
						{Name: "addon", File: "cm.yaml", Content: "kind: ConfigMap"},
					},
				},
			}
			out, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", "vsphere://x"), charCluster(true))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("addon/cm.yaml"), "the manifest path must be rendered for "+dist)
		})
	}
}
