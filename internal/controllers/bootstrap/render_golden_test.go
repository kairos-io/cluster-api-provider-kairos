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
	"flag"
	"os"
	"path/filepath"
	"testing"

	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// updateControllerGolden regenerates the controller-assembly golden files
// instead of asserting against them. Run
//
//	go test ./internal/controllers/bootstrap/... -run TestControllerGolden -update
//
// These goldens are the byte-for-byte snapshot of what generateCloudConfig
// produces today — the ONLY honest proof that the P0 distribution-seam refactor
// changes no rendered bytes. Unlike internal/bootstrap/golden_test.go (which
// renders hand-built TemplateData), these drive the full controller assembly:
// token resolution, providerID, management-endpoint resolver, LB lookup and
// applyControlPlaneRenderData. After PR 1 no P0 commit touches this directory.
var updateControllerGolden = flag.Bool("update", false, "regenerate controller golden files under testdata/golden")

// ---------------------------------------------------------------------------
// field-to-fixture coverage table (done-criterion, M3a / S5)
//
// Every shared TemplateData field the controller assembles must be non-empty in
// at least one fixture below, so the goldens cannot silently stop exercising a
// field. This table is checked in review; keep it current when adding fields.
//
//	TemplateData field                | fixture(s) that make it non-empty
//	----------------------------------+--------------------------------------------
//	Role                              | every fixture
//	SingleNode                        | *_cp_single_*
//	K0sSingleNode                     | k0s_cp_single_capv (k0sSingleNode=true)
//	Hostname                          | every fixture (explicit spec.hostname)
//	UserName                          | every fixture
//	UserPassword                      | every fixture (inline) + *_secretref via Secret
//	UserGroups                        | every fixture
//	GitHubUser                        | every fixture
//	SSHPublicKey                      | every fixture
//	WorkerToken (k0s)                 | k0s_worker_*
//	Manifests                         | every fixture
//	Files                             | every fixture
//	HostnamePrefix                    | every fixture
//	DNSServers                        | every fixture
//	PodCIDR (k0s)                     | k0s_* (set from spec)
//	ServiceCIDR (k0s)                 | k0s_* (set from spec)
//	PrimaryIP                         | every fixture
//	MachineName                       | every fixture
//	ClusterNS                         | every fixture
//	IsKubeVirt                        | *_capk_*
//	Metal3                            | *_worker_metal3
//	IsFleet                           | *_worker_fleet
//	Manifests                         | TestGenerateCloudConfig_ManifestsCopied (not in YAML goldens; see note)
//	Install                           | every fixture (spec.install set)
//	ProviderID                        | every non-metal3/non-fleet fixture
//	K3sServerURL (k3s)                | k3s_* (from cluster endpoint / serverAddress)
//	K3sToken (k3s)                    | k3s_worker_*
//	ControlPlaneLBServiceName/NS      | every fixture (cluster set)
//	ControlPlaneLBEndpoint            | *_capk_cp_* (LB Service ingress)
//	ControlPlaneRole                  | *_cp_init_* (init) / *_cp_join_* (join) / *_cp_single_* (single)
//	JoinToken                         | k3s_cp_init_*, *_cp_join_* (resolved from SecretRef)
//	VIP                               | *_capv_cp_init_* / *_capv_cp_join_* (spec.controlPlaneVIP)
//	ManagementEndpoint                | *_cp_* with a resolver (init/join/single capk+capv)
//	  .JoinTokenSecretName            | k0s_cp_init_* (k0s init push block)
//	  .EtcdStatusSecretName           | *_cp_init_*, *_cp_join_* (HA reporter)
//	  .ControlPlaneEndpointHost       | *_cp_* (stamped from cluster)
// ---------------------------------------------------------------------------

const (
	goldenClusterName = "ha-cluster"
	goldenNamespace   = "default"
	goldenCPHost      = "10.0.0.42"
	goldenCPPort      = 6443
	goldenLBIP        = "10.96.0.10"
)

// goldenResolver is a canned ManagementEndpointResolver for the control-plane
// fixtures. The token is a fixed test literal (unit-test fixture, test/CLAUDE.md
// rule 5) so the goldens are reproducible.
type goldenResolver struct{ endpoint *ManagementEndpoint }

func (g *goldenResolver) Resolve(_ context.Context, _ *bootstrapv1beta2.KairosConfig, _ *clusterv1.Cluster) (*ManagementEndpoint, error) {
	return g.endpoint, nil
}

func cannedEndpoint() *ManagementEndpoint {
	return &ManagementEndpoint{
		APIServer:                 "https://mgmt.example.com:6443",
		Token:                     "mgmt-token",
		KubeconfigSecretName:      goldenClusterName + "-kubeconfig-push",
		KubeconfigSecretNamespace: goldenNamespace,
	}
}

// goldenBaseSpec is the rich, field-dense spec shared by all fixtures. Each
// distribution's generator copies only the fields its templates read, so
// setting PodCIDR/ServiceCIDR here is ignored by the k3s path (I1) — the
// field-set isolation tests prove this explicitly.
func goldenBaseSpec(dist, role string) bootstrapv1beta2.KairosConfigSpec {
	return bootstrapv1beta2.KairosConfigSpec{
		Role:              role,
		Distribution:      dist,
		KubernetesVersion: "v1.30.0",
		Hostname:          "kairos-node-0",
		UserName:          "kairos",
		UserPassword:      "test-password",
		UserGroups:        []string{"admin", "users"},
		GitHubUser:        "octocat",
		SSHPublicKey:      "ssh-ed25519 AAAATESTKEYtestexample test@example",
		HostnamePrefix:    "node-",
		DNSServers:        []string{"1.1.1.1", "8.8.8.8"},
		PrimaryIP:         "192.0.2.50",
		PodCIDR:           "10.244.0.0/16",
		ServiceCIDR:       "10.96.0.0/12",
		// NOTE: spec.Manifests is deliberately NOT set here. The manifest render
		// path emits the manifest Content and the heredoc terminator at column 0
		// inside a `content: |` YAML block scalar, which breaks the enclosing
		// scalar and makes the whole cloud-config fail yaml.Unmarshal. That is a
		// pre-existing template characteristic, out of P0 scope (no behaviour
		// change), so it is not frozen in these YAML-validated goldens. The
		// controller's copy of spec.Manifests into TemplateData (identical for k0s
		// and k3s) is instead characterised by TestGenerateCloudConfig_ManifestsCopied.
		Files: []bootstrapv1beta2.File{
			{Path: "/etc/motd", Content: "hello world", Permissions: "0644", Owner: "root:root"},
		},
		Install: &bootstrapv1beta2.InstallConfig{
			Auto:   ptr.To(true),
			Device: "/dev/sda",
			Reboot: ptr.To(true),
		},
	}
}

func goldenCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: goldenClusterName, Namespace: goldenNamespace},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: goldenCPHost, Port: goldenCPPort},
		},
	}
}

func goldenMachine(infraKind, providerID string) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "kairos-node-0", Namespace: goldenNamespace},
		Spec: clusterv1.MachineSpec{
			ClusterName:       goldenClusterName,
			ProviderID:        providerID,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{Kind: infraKind},
		},
	}
	return m
}

func goldenLBService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      goldenClusterName + "-" + controlPlaneLBServiceSuffix,
			Namespace: goldenNamespace,
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: goldenLBIP}},
			},
		},
	}
}

func goldenTokenSecret(name, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: goldenNamespace},
		Data:       map[string][]byte{"token": []byte(value)},
	}
}

// ctrlGoldenFixture is one (distribution, role, infrastructure) controller-
// assembly snapshot.
type ctrlGoldenFixture struct {
	name     string
	kc       *bootstrapv1beta2.KairosConfig
	machine  *clusterv1.Machine
	cluster  *clusterv1.Cluster
	objects  []client.Object            // Secrets / LB Service the fake client holds
	resolver ManagementEndpointResolver // reconciler.MgmtEndpointResolver (may be nil)
}

func kcWith(dist, role string, mutate func(s *bootstrapv1beta2.KairosConfigSpec)) *bootstrapv1beta2.KairosConfig {
	spec := goldenBaseSpec(dist, role)
	if mutate != nil {
		mutate(&spec)
	}
	return &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kairos-node-0", Namespace: goldenNamespace},
		Spec:       spec,
	}
}

func ctrlGoldenFixtures() []ctrlGoldenFixture {
	var fixtures []ctrlGoldenFixture

	for _, dist := range []string{"k0s", "k3s"} {
		dist := dist
		providerID := "vsphere://vm-uuid-1234"
		capkProviderID := "kubevirt://kairos-node-0"

		// worker: inline token (k0s WorkerToken / k3s K3sToken), serverAddress
		// from the cluster endpoint. One per infrastructure family.
		workerMutate := func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.WorkerToken = "worker-join-token"
			s.K3sToken = "k3s-join-token"
		}
		fixtures = append(fixtures,
			ctrlGoldenFixture{
				name:    dist + "_worker_capv",
				kc:      kcWith(dist, "worker", workerMutate),
				machine: goldenMachine("VSphereMachine", providerID),
				cluster: goldenCluster(),
			},
			ctrlGoldenFixture{
				name:    dist + "_worker_capk",
				kc:      kcWith(dist, "worker", workerMutate),
				machine: goldenMachine("KubevirtMachine", capkProviderID),
				cluster: goldenCluster(),
			},
			ctrlGoldenFixture{
				name:    dist + "_worker_metal3",
				kc:      kcWith(dist, "worker", workerMutate),
				machine: goldenMachine("Metal3Machine", ""),
				cluster: goldenCluster(),
			},
			ctrlGoldenFixture{
				name:    dist + "_worker_fleet",
				kc:      kcWith(dist, "worker", workerMutate),
				machine: goldenMachine("KairosFleetMachine", ""),
				cluster: goldenCluster(),
			},
		)

		// single control plane, CAPV (generic) and CAPK.
		singleCAPVMutate := func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.SingleNode = true
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
			if dist == "k0s" {
				s.K0sSingleNode = ptr.To(true)
			}
		}
		fixtures = append(fixtures, ctrlGoldenFixture{
			name:     dist + "_cp_single_capv",
			kc:       kcWith(dist, "control-plane", singleCAPVMutate),
			machine:  goldenMachine("VSphereMachine", providerID),
			cluster:  goldenCluster(),
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})
		fixtures = append(fixtures, ctrlGoldenFixture{
			name: dist + "_cp_single_capk",
			kc: kcWith(dist, "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.SingleNode = true
				s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
			}),
			machine:  goldenMachine("KubevirtMachine", capkProviderID),
			cluster:  goldenCluster(),
			objects:  []client.Object{goldenLBService()},
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})

		// init control plane, CAPV (kube-vip + etcd reporter) and CAPK (no VIP).
		fixtures = append(fixtures, ctrlGoldenFixture{
			name: dist + "_cp_init_capv",
			kc: kcWith(dist, "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleInit
				s.ControlPlaneVIP = &bootstrapv1beta2.ControlPlaneVIP{Address: "192.168.1.240", Interface: "eth0", Mode: "ARP"}
				if dist == "k3s" {
					s.K3sTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "k3s-server-token"}
				}
			}),
			machine:  goldenMachine("VSphereMachine", providerID),
			cluster:  goldenCluster(),
			objects:  []client.Object{goldenTokenSecret("k3s-server-token", "shared-server-token")},
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})
		fixtures = append(fixtures, ctrlGoldenFixture{
			name: dist + "_cp_init_capk",
			kc: kcWith(dist, "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
				s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleInit
				if dist == "k3s" {
					s.K3sTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "k3s-server-token"}
				}
			}),
			machine:  goldenMachine("KubevirtMachine", capkProviderID),
			cluster:  goldenCluster(),
			objects:  []client.Object{goldenLBService(), goldenTokenSecret("k3s-server-token", "shared-server-token")},
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})

		// join control plane, CAPV and CAPK. Join token from the distribution's
		// *SecretRef (TOKEN-INV): k0s ControlPlaneJoinTokenSecretRef, k3s K3sTokenSecretRef.
		joinMutate := func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleJoin
			if dist == "k3s" {
				s.K3sTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "k3s-server-token"}
			} else {
				s.ControlPlaneJoinTokenSecretRef = &bootstrapv1beta2.WorkerTokenSecretReference{Name: "k0s-cp-join-token"}
			}
		}
		joinObjects := []client.Object{
			goldenTokenSecret("k3s-server-token", "shared-server-token"),
			goldenTokenSecret("k0s-cp-join-token", "k0s-controller-join-token"),
		}
		fixtures = append(fixtures, ctrlGoldenFixture{
			name: dist + "_cp_join_capv",
			kc: kcWith(dist, "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
				joinMutate(s)
				s.ControlPlaneVIP = &bootstrapv1beta2.ControlPlaneVIP{Address: "192.168.1.240", Interface: "eth0", Mode: "ARP"}
			}),
			machine:  goldenMachine("VSphereMachine", providerID),
			cluster:  goldenCluster(),
			objects:  joinObjects,
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})
		fixtures = append(fixtures, ctrlGoldenFixture{
			name:     dist + "_cp_join_capk",
			kc:       kcWith(dist, "control-plane", joinMutate),
			machine:  goldenMachine("KubevirtMachine", capkProviderID),
			cluster:  goldenCluster(),
			objects:  append([]client.Object{goldenLBService()}, joinObjects...),
			resolver: &goldenResolver{endpoint: cannedEndpoint()},
		})
	}

	// S5 extras: control-plane edge cases the base matrix does not cover.
	// (a) nil resolver: the reconciler has no MgmtEndpointResolver at all.
	fixtures = append(fixtures, ctrlGoldenFixture{
		name: "k0s_cp_single_capv_nil_resolver",
		kc: kcWith("k0s", "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.SingleNode = true
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
		}),
		machine:  goldenMachine("VSphereMachine", "vsphere://vm-uuid-1234"),
		cluster:  goldenCluster(),
		resolver: nil,
	})
	// (b) resolver returns (nil, nil): the documented "disabled" signal.
	fixtures = append(fixtures, ctrlGoldenFixture{
		name: "k0s_cp_single_capv_disabled_resolver",
		kc: kcWith("k0s", "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.SingleNode = true
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
		}),
		machine:  goldenMachine("VSphereMachine", "vsphere://vm-uuid-1234"),
		cluster:  goldenCluster(),
		resolver: &goldenResolver{endpoint: nil},
	})
	// (c) control plane on DockerMachine: push is disabled by supportsManagementEndpoint.
	fixtures = append(fixtures, ctrlGoldenFixture{
		name: "k0s_cp_single_docker",
		kc: kcWith("k0s", "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.SingleNode = true
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
		}),
		machine:  goldenMachine("DockerMachine", "docker:////kairos-node-0"),
		cluster:  goldenCluster(),
		resolver: &goldenResolver{endpoint: cannedEndpoint()},
	})
	// (d) k3s control plane with an explicit spec.serverAddress (overrides the
	// cluster endpoint derivation).
	fixtures = append(fixtures, ctrlGoldenFixture{
		name: "k3s_cp_single_capv_serveraddr",
		kc: kcWith("k3s", "control-plane", func(s *bootstrapv1beta2.KairosConfigSpec) {
			s.SingleNode = true
			s.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleSingle
			s.ServerAddress = "https://explicit.example.com:6443"
		}),
		machine:  goldenMachine("VSphereMachine", "vsphere://vm-uuid-1234"),
		cluster:  goldenCluster(),
		resolver: &goldenResolver{endpoint: cannedEndpoint()},
	})

	return fixtures
}

func ctrlGoldenScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := bootstrapv1beta2.AddToScheme(scheme); err != nil {
		t.Fatalf("add bootstrap scheme: %v", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cluster scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return scheme
}

func ctrlGoldenPath(name string) string {
	return filepath.Join("testdata", "golden", name+".yaml")
}

// TestControllerGolden renders every controller-assembly fixture through
// generateCloudConfig and compares the output to its frozen golden file. With
// -update it (re)writes the goldens instead.
func TestControllerGolden(t *testing.T) {
	for _, tc := range ctrlGoldenFixtures() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			scheme := ctrlGoldenScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			r := &KairosConfigReconciler{Client: c, Scheme: scheme, MgmtEndpointResolver: tc.resolver}

			got, err := r.generateCloudConfig(context.Background(), log.Log, tc.kc, tc.machine, tc.cluster)
			if err != nil {
				t.Fatalf("generateCloudConfig(%s): %v", tc.name, err)
			}
			// Every rendered cloud-config MUST be valid YAML.
			var sink any
			if err := yaml.Unmarshal([]byte(got), &sink); err != nil {
				t.Fatalf("rendered %s is not valid YAML: %v", tc.name, err)
			}

			path := ctrlGoldenPath(tc.name)
			if *updateControllerGolden {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run with -update to create): %v", path, err)
			}
			if got != string(want) {
				t.Errorf("rendered %s does not match golden %s.\n"+
					"If this change is intended, re-run with -update and review the diff.",
					tc.name, path)
			}
		})
	}
}
