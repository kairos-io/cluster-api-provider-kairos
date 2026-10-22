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
	"regexp"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// testTokenNow is the fixed clock buildSource installs on the kubeadm source.
var testTokenNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// renderedTokenPattern pulls the bootstrap token out of a rendered JoinConfiguration.
var renderedTokenPattern = regexp.MustCompile(`token: ([a-z0-9]{6}\.[a-z0-9]{16})`)

func renderedToken(t *testing.T, s string) string {
	t.Helper()
	m := renderedTokenPattern.FindStringSubmatch(s)
	if len(m) != 2 {
		t.Fatalf("no bootstrap token in rendered output")
	}
	return m[1]
}

// workloadTokenIDs lists the IDs of the bootstrap token Secrets in the workload cluster.
func workloadTokenIDs(t *testing.T, wc client.Client) []string {
	t.Helper()
	list := &corev1.SecretList{}
	if err := wc.List(context.Background(), list, client.InNamespace(metav1.NamespaceSystem)); err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := []string{}
	for _, s := range list.Items {
		ids = append(ids, string(s.Data[bootstrapapi.BootstrapTokenIDKey]))
	}
	return ids
}

func getWorkloadToken(t *testing.T, wc client.Client, id string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := wc.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + id}, s); err != nil {
		t.Fatalf("get token %s: %v", id, err)
	}
	return s
}

func setWorkloadTokenExpiry(t *testing.T, wc client.Client, id string, exp []byte) {
	t.Helper()
	s := getWorkloadToken(t, wc, id)
	if exp == nil {
		delete(s.Data, bootstrapapi.BootstrapTokenExpirationKey)
	} else {
		s.Data[bootstrapapi.BootstrapTokenExpirationKey] = exp
	}
	if err := wc.Update(context.Background(), s); err != nil {
		t.Fatalf("update token: %v", err)
	}
}

// TestKubeadmWorkerJoin_ReusesRecordedToken pins the core of the fix: a second
// render of the same KairosConfig (the providerID regeneration, a deleted data
// Secret) carries the token recorded in status, because the VM may already hold
// it. Only a token that is gone from the workload cluster is replaced.
func TestKubeadmWorkerJoin_ReusesRecordedToken(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")

	t.Run("recorded token is reused and extended, never re-minted", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		first, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		id := first.Kubeadm.TokenID
		token := renderedToken(t, first.Kubeadm.JoinConfiguration)

		// Ten minutes later the token has five left; the re-render must keep it.
		later := testTokenNow.Add(10 * time.Minute)
		src.now = func() time.Time { return later }
		req.Config.Status.BootstrapTokenID = id
		second, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(second.Kubeadm.TokenID).To(Equal(id))
		g.Expect(second.Kubeadm.ReplacedTokenID).To(BeEmpty())
		g.Expect(renderedToken(t, second.Kubeadm.JoinConfiguration)).To(Equal(token))
		g.Expect(workloadTokenIDs(t, workload)).To(ConsistOf(id))
		exp := getWorkloadToken(t, workload, id).Data[bootstrapapi.BootstrapTokenExpirationKey]
		g.Expect(string(exp)).To(Equal(later.Add(defaultKubeadmTokenTTL).Format(time.RFC3339)))
	})

	t.Run("nothing recorded yet (stale status) -> the token minted for this KairosConfig is found and reused", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		req.Config.UID = "kc-uid"
		first, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(getWorkloadToken(t, workload, first.Kubeadm.TokenID).Labels).To(HaveKeyWithValue(kubeadmTokenOwnerLabel, "kc-uid"))

		// A second reconcile that read the KairosConfig before the first one's
		// status write landed: BootstrapTokenID is still empty.
		second, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(second.Kubeadm.TokenID).To(Equal(first.Kubeadm.TokenID))
		g.Expect(renderedToken(t, second.Kubeadm.JoinConfiguration)).To(Equal(renderedToken(t, first.Kubeadm.JoinConfiguration)))
		g.Expect(workloadTokenIDs(t, workload)).To(ConsistOf(first.Kubeadm.TokenID))

		// Another KairosConfig never picks it up.
		other := req
		other.Config = req.Config.DeepCopy()
		other.Config.UID = "other-uid"
		third, err := src.WorkerJoin(context.Background(), other)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(third.Kubeadm.TokenID).NotTo(Equal(first.Kubeadm.TokenID))
	})

	t.Run("recorded token gone -> replacement minted and reported", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		first, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		id := first.Kubeadm.TokenID
		g.Expect(workload.Delete(context.Background(), getWorkloadToken(t, workload, id))).To(Succeed())

		req.Config.Status.BootstrapTokenID = id
		second, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(second.Kubeadm.TokenID).NotTo(Equal(id))
		g.Expect(second.Kubeadm.ReplacedTokenID).To(Equal(id))
		g.Expect(workloadTokenIDs(t, workload)).To(ConsistOf(second.Kubeadm.TokenID))
	})

	t.Run("recorded ID points at a Secret that is not a bootstrap token -> replaced", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		g.Expect(workload.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-abcdef"},
			Type:       corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				bootstrapapi.BootstrapTokenIDKey:     []byte("abcdef"),
				bootstrapapi.BootstrapTokenSecretKey: []byte("0123456789abcdef"),
			},
		})).To(Succeed())
		req.Config.Status.BootstrapTokenID = "abcdef"
		got, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got.Kubeadm.TokenID).NotTo(Equal("abcdef"))
		g.Expect(got.Kubeadm.ReplacedTokenID).To(Equal("abcdef"))
	})
}

// TestKubeadmExtendWorkerToken pins the in-place extension: the expiry moves, the
// token's ID and secret half do not, and a token that is gone is reported so the
// caller can re-render.
func TestKubeadmExtendWorkerToken(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")

	mint := func(t *testing.T, fx kubeadmFixture) (*kubeadmJoinSource, JoinRequest, client.Client, string) {
		t.Helper()
		src, req, workload := buildSource(t, fx, nil)
		m, err := src.WorkerJoin(context.Background(), req)
		if err != nil {
			t.Fatalf("WorkerJoin: %v", err)
		}
		return src, req, workload, m.Kubeadm.TokenID
	}

	t.Run("near expiry -> expiry moved, token untouched", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload, id := mint(t, defaultFixture(ca))
		before := getWorkloadToken(t, workload, id)
		later := testTokenNow.Add(12 * time.Minute)
		src.now = func() time.Time { return later }

		found, err := src.ExtendWorkerToken(context.Background(), req, id)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		after := getWorkloadToken(t, workload, id)
		g.Expect(string(after.Data[bootstrapapi.BootstrapTokenExpirationKey])).To(Equal(later.Add(defaultKubeadmTokenTTL).Format(time.RFC3339)))
		g.Expect(after.Data[bootstrapapi.BootstrapTokenSecretKey]).To(Equal(before.Data[bootstrapapi.BootstrapTokenSecretKey]))
		g.Expect(workloadTokenIDs(t, workload)).To(ConsistOf(id))
	})

	t.Run("fresh -> left alone", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload, id := mint(t, defaultFixture(ca))
		before := getWorkloadToken(t, workload, id)
		src.now = func() time.Time { return testTokenNow.Add(1 * time.Minute) }

		found, err := src.ExtendWorkerToken(context.Background(), req, id)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		after := getWorkloadToken(t, workload, id)
		g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "a token good for most of its TTL is not rewritten")
	})

	t.Run("no expiration key -> ours is written", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload, id := mint(t, defaultFixture(ca))
		setWorkloadTokenExpiry(t, workload, id, nil)
		found, err := src.ExtendWorkerToken(context.Background(), req, id)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(string(getWorkloadToken(t, workload, id).Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(testTokenNow.Add(defaultKubeadmTokenTTL).Format(time.RFC3339)))
	})

	t.Run("unparsable expiration -> ours is written", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload, id := mint(t, defaultFixture(ca))
		setWorkloadTokenExpiry(t, workload, id, []byte("not-a-time"))
		found, err := src.ExtendWorkerToken(context.Background(), req, id)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(string(getWorkloadToken(t, workload, id).Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(testTokenNow.Add(defaultKubeadmTokenTTL).Format(time.RFC3339)))
	})

	t.Run("gone -> not found, nothing minted", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		found, err := src.ExtendWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeFalse())
		g.Expect(workloadTokenIDs(t, workload)).To(BeEmpty())
	})

	t.Run("empty or malformed ID -> not found, workload not contacted", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.workloadClient = func(context.Context, client.ObjectKey) (client.Client, error) {
			t.Fatalf("workload cluster must not be contacted")
			return nil, nil
		}
		for _, id := range []string{"", "ABCDEF", "abc", "../x"} {
			found, err := src.ExtendWorkerToken(context.Background(), req, id)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeFalse())
		}
	})

	t.Run("trust check re-runs: a kubeconfig no longer owned by the control plane is refused", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.ownerUID = types.UID("someone-else")
		src, req, workload := buildSource(t, fx, nil)
		_, err := src.ExtendWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must be owned by the cluster's control plane"))
		g.Expect(workloadTokenIDs(t, workload)).To(BeEmpty())
	})

	t.Run("workload unreachable -> not ready", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.workloadClient = func(context.Context, client.ObjectKey) (client.Client, error) {
			return nil, errors.New("connection refused")
		}
		_, err := src.ExtendWorkerToken(context.Background(), req, "abcdef")
		var nr *bootstrapNotReadyError
		g.Expect(errors.As(err, &nr)).To(BeTrue())
	})
}

// TestKubeadmRevokeWorkerToken pins revocation: the token Secret is deleted, and a
// token that is already gone is success.
func TestKubeadmRevokeWorkerToken(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")

	t.Run("deletes the token", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		m, err := src.WorkerJoin(context.Background(), req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(src.RevokeWorkerToken(context.Background(), req, m.Kubeadm.TokenID)).To(Succeed())
		g.Expect(workloadTokenIDs(t, workload)).To(BeEmpty())
	})
	t.Run("already gone -> success", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		g.Expect(src.RevokeWorkerToken(context.Background(), req, "abcdef")).To(Succeed())
	})
	t.Run("malformed ID -> no-op", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.workloadClient = func(context.Context, client.ObjectKey) (client.Client, error) {
			t.Fatalf("workload cluster must not be contacted")
			return nil, nil
		}
		g.Expect(src.RevokeWorkerToken(context.Background(), req, "../kube-root-ca")).To(Succeed())
	})
}

// TestKubeadmTokenRecheckInterval pins the requeue cadence: a third of the TTL
// (CABPK), floored.
func TestKubeadmTokenRecheckInterval(t *testing.T) {
	g := NewWithT(t)
	g.Expect((&kubeadmJoinSource{}).TokenRecheckInterval()).To(Equal(5 * time.Minute))
	g.Expect((&kubeadmJoinSource{tokenTTL: 3 * time.Second}).TokenRecheckInterval()).To(Equal(minKubeadmTokenRecheckInterval))
}

// fakeRefresher is a JoinMaterialSource + workerTokenRefresher test double that
// records what the reconciler asked of it.
type fakeRefresher struct {
	found     bool
	extendErr error
	revokeErr error
	extended  []string
	revoked   []string
}

func (f *fakeRefresher) WorkerJoin(context.Context, JoinRequest) (WorkerJoinMaterial, error) {
	return WorkerJoinMaterial{Kubeadm: &KubeadmJoinMaterial{JoinConfiguration: "x", TokenID: "abcdef"}}, nil
}
func (f *fakeRefresher) ExtendWorkerToken(_ context.Context, _ JoinRequest, id string) (bool, error) {
	f.extended = append(f.extended, id)
	return f.found, f.extendErr
}
func (f *fakeRefresher) RevokeWorkerToken(_ context.Context, _ JoinRequest, id string) error {
	f.revoked = append(f.revoked, id)
	return f.revokeErr
}
func (f *fakeRefresher) TokenRecheckInterval() time.Duration { return 5 * time.Minute }

// TestReconcileKubeadmWorkerToken pins the reconciler's lifecycle gate: extend
// while booting, re-render only when nothing is left to extend, recover a missing
// ID from the data Secret, revoke after nodeRef, cap an old Machine, and leave k0s
// alone.
func TestReconcileKubeadmWorkerToken(t *testing.T) {
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	worker := func(id string) *bootstrapv1beta2.KairosConfig {
		return &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", UID: "kc-uid"},
			Spec:       bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"},
			Status:     bootstrapv1beta2.KairosConfigStatus{BootstrapTokenID: id},
		}
	}
	ownedData := func(value string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{Kind: "KairosConfig", Name: "w", UID: "kc-uid"}}},
			Data: map[string][]byte{"value": []byte(value)},
		}
	}
	booting := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))}}
	old := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	joined := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))},
		Status:     clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: "node-w"}},
	}
	reconciler := func(ref *fakeRefresher) *KairosConfigReconciler {
		return &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now },
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
	}
	run := func(r *KairosConfigReconciler, kc *bootstrapv1beta2.KairosConfig, m *clusterv1.Machine, data *corev1.Secret) (kubeadmRefreshDecision, error) {
		return r.reconcileKubeadmWorkerToken(context.Background(), logr.Discard(), kc, m, nil, data)
	}

	t.Run("booting, token present -> extended, no re-render", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: true}
		kc := worker("abcdef")
		d, err := run(reconciler(ref), kc, booting, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
		g.Expect(ref.extended).To(Equal([]string{"abcdef"}))
		g.Expect(ref.revoked).To(BeEmpty())
		g.Expect(kc.Status.BootstrapTokenID).To(Equal("abcdef"))
	})
	t.Run("booting, token gone -> regenerate", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: false}
		d, err := run(reconciler(ref), worker("abcdef"), booting, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRegenerate))
	})
	t.Run("booting, workload not ready -> error surfaces as not ready", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{extendErr: newNotReady("WorkloadClusterUnreachable", "x")}
		_, err := run(reconciler(ref), worker("abcdef"), booting, nil)
		var nr *bootstrapNotReadyError
		g.Expect(errors.As(err, &nr)).To(BeTrue())
	})
	t.Run("upgrade: no stored ID -> recovered from the data Secret and extended", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: true}
		kc := worker("")
		data := ownedData(kubeadmBootstrapDataWithToken("qrstuv.0123456789abcdef"))
		d, err := run(reconciler(ref), kc, booting, data)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone), "the recovered token must be extended, not replaced")
		g.Expect(kc.Status.BootstrapTokenID).To(Equal("qrstuv"))
		g.Expect(ref.extended).To(Equal([]string{"qrstuv"}))
	})
	t.Run("upgrade: a data Secret this KairosConfig does not own is not trusted", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: true}
		kc := worker("")
		data := ownedData(kubeadmBootstrapDataWithToken("qrstuv.0123456789abcdef"))
		data.OwnerReferences = nil
		d, err := run(reconciler(ref), kc, booting, data)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRegenerate))
		g.Expect(kc.Status.BootstrapTokenID).To(BeEmpty())
		g.Expect(ref.extended).To(BeEmpty())
	})
	t.Run("upgrade: no stored ID and none in the data Secret -> regenerate", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: true}
		data := ownedData("#cloud-config\nhostname: x\n")
		d, err := run(reconciler(ref), worker(""), booting, data)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRegenerate))
		g.Expect(ref.extended).To(BeEmpty())
	})
	t.Run("joined -> revoked and ID cleared", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		kc := worker("abcdef")
		d, err := run(reconciler(ref), kc, joined, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
		g.Expect(ref.revoked).To(Equal([]string{"abcdef"}))
		g.Expect(ref.extended).To(BeEmpty())
		g.Expect(kc.Status.BootstrapTokenID).To(BeEmpty())
	})
	t.Run("joined, revoke not ready -> ID kept for the retry", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{revokeErr: newNotReady("TokenRevokeFailed", "x")}
		kc := worker("abcdef")
		_, err := run(reconciler(ref), kc, joined, nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(kc.Status.BootstrapTokenID).To(Equal("abcdef"))
	})
	t.Run("joined, trust refused -> not failed, ID forgotten", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{revokeErr: errors.New("refused")}
		kc := worker("abcdef")
		_, err := run(reconciler(ref), kc, joined, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(kc.Status.BootstrapTokenID).To(BeEmpty())
	})
	t.Run("joined, nothing recorded -> nothing to do", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		_, err := run(reconciler(ref), worker(""), joined, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref.revoked).To(BeEmpty())
	})
	t.Run("past the cap -> capped, not extended", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{found: true}
		d, err := run(reconciler(ref), worker("abcdef"), old, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshCapped))
		g.Expect(ref.extended).To(BeEmpty())
	})
	t.Run("k0s worker -> untouched", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		kc := worker("abcdef")
		kc.Spec.Distribution = bootstrapv1beta2.DistributionK0s
		d, err := run(reconciler(ref), kc, booting, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
		g.Expect(ref.extended).To(BeEmpty())
	})
}

// TestKubeadmTokenRecheck pins the requeue Reconcile asks for: on the token's
// clock while the Machine has not joined or a joined Machine still has a token to
// revoke, and none otherwise.
func TestKubeadmTokenRecheck(t *testing.T) {
	g := NewWithT(t)
	r := &KairosConfigReconciler{JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: &fakeRefresher{}}}
	kc := &bootstrapv1beta2.KairosConfig{Spec: bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"}}
	booting := &clusterv1.Machine{}
	joined := &clusterv1.Machine{Status: clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: "n"}}}

	g.Expect(r.kubeadmTokenRecheck(kc, booting)).To(Equal(5 * time.Minute))
	g.Expect(r.kubeadmTokenRecheck(kc, joined)).To(BeZero())
	kc.Status.BootstrapTokenID = "abcdef"
	g.Expect(r.kubeadmTokenRecheck(kc, joined)).To(Equal(5*time.Minute), "a joined Machine with a token left to revoke")
	g.Expect(r.kubeadmTokenRecheck(kc, nil)).To(BeZero())
	k0s := kc.DeepCopy()
	k0s.Spec.Distribution = bootstrapv1beta2.DistributionK0s
	g.Expect(r.kubeadmTokenRecheck(k0s, booting)).To(BeZero())
}

// kubeadmBootstrapDataWithToken is a minimal rendered kubeadm cloud-config
// carrying token in the JoinConfiguration file, shaped as the template writes it.
func kubeadmBootstrapDataWithToken(token string) string {
	return "#cloud-config\n" +
		"write_files:\n" +
		"  - path: /etc/kubernetes/kairos-kubeadm-join.yaml\n" +
		"    permissions: \"0600\"\n" +
		"    owner: root:root\n" +
		"    content: |\n" +
		"      apiVersion: kubeadm.k8s.io/v1beta3\n" +
		"      kind: JoinConfiguration\n" +
		"      discovery:\n" +
		"        bootstrapToken:\n" +
		"          apiServerEndpoint: 10.0.0.1:6443\n" +
		"          token: " + token + "\n"
}

// TestBootstrapTokenIDFromBootstrapData pins the upgrade-path parser against both
// a hand-written fixture and the real renderer, so a template change that moves
// the JoinConfiguration breaks this test rather than silently disabling recovery.
func TestBootstrapTokenIDFromBootstrapData(t *testing.T) {
	g := NewWithT(t)
	g.Expect(bootstrapTokenIDFromBootstrapData([]byte(kubeadmBootstrapDataWithToken("qrstuv.0123456789abcdef")))).To(Equal("qrstuv"))
	g.Expect(bootstrapTokenIDFromBootstrapData([]byte(kubeadmBootstrapDataWithToken("not-a-token")))).To(BeEmpty())
	g.Expect(bootstrapTokenIDFromBootstrapData([]byte("#cloud-config\nhostname: x\n"))).To(BeEmpty())
	g.Expect(bootstrapTokenIDFromBootstrapData([]byte("::: not yaml"))).To(BeEmpty())

	// Real render through the reconciler and the kubeadm source.
	ca := genTestCA(t, "cluster-ca")
	src, req, _ := buildSource(t, defaultFixture(ca), nil)
	scheme := charScheme(t)
	r := &KairosConfigReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme,
		JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: src}}
	kc := req.Config.DeepCopy()
	kc.Spec.UserName = "kairos"
	kc.Spec.UserPassword = "pw"
	cc, mat, err := r.renderCloudConfigM(context.Background(), logr.Discard(), bootstrapv1beta2.DistributionKubeadm,
		kc, req.Machine, req.Cluster, "worker", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(bootstrapTokenIDFromBootstrapData([]byte(cc))).To(Equal(mat.Kubeadm.TokenID))
}

// TestReconcileBootstrapData_KubeadmProviderIDRegenerationReusesToken reproduces
// the 2026-10-16 CAPK A/B failure at the reconciler: the first render records
// token A, the infrastructure provider publishes a providerID, and the
// regeneration that embeds it must carry token A again rather than mint token B.
func TestReconcileBootstrapData_KubeadmProviderIDRegenerationReusesToken(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, workload := buildSource(t, defaultFixture(ca), nil)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme,
		JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: src}}

	kc := req.Config.DeepCopy()
	kc.UID = "kc-uid"
	kc.Spec.UserName = "kairos"
	kc.Spec.UserPassword = "pw"
	machine := req.Machine.DeepCopy()
	cluster := req.Cluster.DeepCopy()
	cluster.Status.Initialization.InfrastructureProvisioned = ptrBool(true)

	_, err := r.reconcileBootstrapData(context.Background(), logr.Discard(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kc.Status.DataSecretName).NotTo(BeNil())
	firstID := kc.Status.BootstrapTokenID
	g.Expect(firstID).NotTo(BeEmpty())
	first := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: kc.Namespace, Name: *kc.Status.DataSecretName}, first)).To(Succeed())
	firstToken := renderedToken(t, string(first.Data["value"]))
	g.Expect(string(first.Data["value"])).NotTo(ContainSubstring("kubevirt://worker-0"))

	// The VM booted from the first render; later the provider publishes a providerID.
	machine.Spec.ProviderID = "kubevirt://worker-0"
	_, err = r.reconcileBootstrapData(context.Background(), logr.Discard(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	second := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: kc.Namespace, Name: *kc.Status.DataSecretName}, second)).To(Succeed())
	g.Expect(string(second.Data["value"])).To(ContainSubstring("kubevirt://worker-0"), "the regeneration must have happened")
	g.Expect(renderedToken(t, string(second.Data["value"]))).To(Equal(firstToken), "the VM holds the first token; the regeneration must carry it")
	g.Expect(kc.Status.BootstrapTokenID).To(Equal(firstID))
	g.Expect(workloadTokenIDs(t, workload)).To(ConsistOf(firstID), "no second token may be minted")
}

// TestReconcileDelete_RevokesKubeadmToken pins revocation on delete, and that it
// never blocks deletion when the workload cluster or the owning objects are gone.
func TestReconcileDelete_RevokesKubeadmToken(t *testing.T) {
	scheme := charScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "wl", Namespace: "default"}}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", UID: "m-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "wl"}},
		Spec: clusterv1.MachineSpec{ClusterName: "wl"},
	}
	kcFor := func() *bootstrapv1beta2.KairosConfig {
		return &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", UID: "kc-uid",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "w", UID: "m-uid",
				}}},
			Spec:   bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"},
			Status: bootstrapv1beta2.KairosConfigStatus{BootstrapTokenID: "abcdef"},
		}
	}
	reconciler := func(ref *fakeRefresher, objs ...client.Object) *KairosConfigReconciler {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		return &KairosConfigReconciler{Client: c, Scheme: scheme,
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
	}

	t.Run("owning Machine and Cluster present -> revoked", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		kc := kcFor()
		_, _, err := reconciler(ref, cluster.DeepCopy(), machine.DeepCopy()).reconcileDelete(context.Background(), logr.Discard(), kc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref.revoked).To(Equal([]string{"abcdef"}))
		g.Expect(kc.Status.BootstrapTokenID).To(BeEmpty())
	})
	t.Run("revoke fails -> deletion still proceeds", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{revokeErr: newNotReady("WorkloadClusterUnreachable", "x")}
		_, _, err := reconciler(ref, cluster.DeepCopy(), machine.DeepCopy()).reconcileDelete(context.Background(), logr.Discard(), kcFor())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref.revoked).To(Equal([]string{"abcdef"}))
	})
	t.Run("owning Machine gone -> skipped, deletion proceeds", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		_, _, err := reconciler(ref).reconcileDelete(context.Background(), logr.Discard(), kcFor())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref.revoked).To(BeEmpty())
	})
}
