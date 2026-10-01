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
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/bootstrap"
)

// bootstrapDistribution is what the bootstrap controller does differently for one
// distribution. It is a static table (bootstrapDistributions), keyed by the api
// distribution constants so it cannot drift from admission or the renderer
// (TestBootstrapDistributionsTableAgrees enforces this).
type bootstrapDistribution struct {
	// fillTemplateData sets the fields that only this distribution's templates
	// read. k0s: WorkerToken, PodCIDR, ServiceCIDR. k3s: K3sToken, and
	// K3sServerURL for every role.
	fillTemplateData func(td *bootstrap.TemplateData, kc *bootstrapv1beta2.KairosConfig, serverAddress string, w WorkerJoinMaterial)
	// builtinJoin builds the default worker join source over the reconciler's
	// client. It is nil when the source needs dependencies only main.go has
	// (kubeadm, in P1).
	builtinJoin func(c client.Reader) JoinMaterialSource
}

// bootstrapDistributions is the controller's distribution table. The keys are the
// api distribution constants; renderCloudConfig rejects any distribution absent
// from this map with the same "unsupported distribution: <name>" error the
// pre-seam switch produced.
var bootstrapDistributions = map[string]bootstrapDistribution{
	bootstrapv1beta2.DistributionK0s: {
		fillTemplateData: func(td *bootstrap.TemplateData, kc *bootstrapv1beta2.KairosConfig, _ string, w WorkerJoinMaterial) {
			td.WorkerToken = w.Token
			td.PodCIDR = kc.Spec.PodCIDR
			td.ServiceCIDR = kc.Spec.ServiceCIDR
		},
		builtinJoin: func(c client.Reader) JoinMaterialSource { return k0sJoinSource{c: c} },
	},
	bootstrapv1beta2.DistributionK3s: {
		fillTemplateData: func(td *bootstrap.TemplateData, _ *bootstrapv1beta2.KairosConfig, serverAddress string, w WorkerJoinMaterial) {
			td.K3sServerURL = serverAddress
			td.K3sToken = w.Token
		},
		builtinJoin: func(c client.Reader) JoinMaterialSource { return k3sJoinSource{c: c} },
	},
	// kubeadm (ADR 0010 P1): builtinJoin is nil — the worker JoinMaterialSource
	// needs dependencies only main.go has (clustercache, an uncached reader, the
	// control-plane GroupKind allowlist), so it is injected via JoinSources and
	// joinSourceFor hard-fails if that wiring is missing (N2). fillTemplateData
	// copies the minted join material from the source and builds the providerID
	// kubeadm patch from the resolved td.ProviderID.
	bootstrapv1beta2.DistributionKubeadm: {
		fillTemplateData: func(td *bootstrap.TemplateData, _ *bootstrapv1beta2.KairosConfig, _ string, w WorkerJoinMaterial) {
			if w.Kubeadm == nil {
				return
			}
			td.Kubeadm = &bootstrap.KubeadmTemplateData{
				JoinConfiguration: w.Kubeadm.JoinConfiguration,
				KubernetesVersion: w.Kubeadm.KubernetesVersion,
				ProviderIDPatch:   kubeadmProviderIDPatch(td.ProviderID),
			}
		},
		builtinJoin: nil,
	},
}

// kubeadmProviderIDPatch returns a kubeadm kubeletconfiguration merge-patch that
// sets the Node providerID (ADR 0010 P1 item 4), or "" when no providerID is known
// (the infra provider owns it, e.g. Metal3, or it is discovered on-node). providerID
// is regex-validated (providerIDPattern) before render, so it contains no
// YAML-special character that double-quoting cannot handle.
func kubeadmProviderIDPatch(providerID string) string {
	if providerID == "" {
		return ""
	}
	return fmt.Sprintf("providerID: %q\n", providerID)
}

// renderCloudConfig is the merged generator for every distribution. It keeps
// today's read order — worker join, password, providerID, management endpoint, LB
// endpoint, then the unchanged applyControlPlaneRenderData — and defers the
// distribution-only field assignment and template selection to the table, so the
// rendered bytes are identical to the pre-seam per-distribution generators.
func (r *KairosConfigReconciler) renderCloudConfig(ctx context.Context, log logr.Logger, dist string, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, role, serverAddress string) (string, error) {
	row, ok := bootstrapDistributions[dist]
	if !ok {
		return "", fmt.Errorf("unsupported distribution: %s", dist)
	}

	// Determine single-node mode. Single-node is determined by the explicit
	// spec.singleNode flag (the owning KairosControlPlane's replicas are reflected
	// into it by the KCP controller). K0sSingleNode only affects the k0s flag.
	singleNode := kairosConfig.Spec.SingleNode
	k0sSingleNode := kairosConfig.Spec.K0sSingleNode != nil && *kairosConfig.Spec.K0sSingleNode
	if !singleNode && role == "control-plane" && machine != nil {
		ownerRef := metav1.GetControllerOf(machine)
		if ownerRef != nil && ownerRef.Kind == "KairosControlPlane" {
			log.V(4).Info("Control plane node, single-node mode determined from spec", "singleNode", singleNode)
		}
	}

	// Worker join material (for worker nodes) through the per-distribution source.
	// It resolves before password/providerID/endpoints and rejects incomplete
	// material; a missing referenced Secret surfaces as errTokenNotReady (requeue).
	var workerMat WorkerJoinMaterial
	if role == "worker" {
		var err error
		workerMat, err = r.workerJoin(ctx, dist, kairosConfig, machine, cluster, serverAddress)
		if err != nil {
			return "", err
		}
	}

	// Set defaults for user configuration.
	userName := kairosConfig.Spec.UserName
	if userName == "" {
		userName = "kairos"
	}
	userPassword, err := r.resolveUserPassword(ctx, kairosConfig)
	if err != nil {
		return "", err
	}
	userGroups := kairosConfig.Spec.UserGroups
	if len(userGroups) == 0 {
		userGroups = []string{"admin"}
	}

	// Set hostname prefix (default to "metal-" if not specified).
	hostnamePrefix := kairosConfig.Spec.HostnamePrefix
	if hostnamePrefix == "" {
		hostnamePrefix = "metal-"
	}

	// Prefer explicit hostname, otherwise use Machine name.
	hostname := kairosConfig.Spec.Hostname
	if hostname == "" && machine != nil {
		hostname = machine.Name
	}

	// Set install configuration (with defaults).
	var installConfig *bootstrap.InstallConfig
	if kairosConfig.Spec.Install != nil {
		installConfig = &bootstrap.InstallConfig{
			Auto:   true,   // Default to true
			Device: "auto", // Default to "auto"
			Reboot: true,   // Default to true
		}
		if kairosConfig.Spec.Install.Auto != nil {
			installConfig.Auto = *kairosConfig.Spec.Install.Auto
		}
		if kairosConfig.Spec.Install.Device != "" {
			installConfig.Device = kairosConfig.Spec.Install.Device
		}
		if kairosConfig.Spec.Install.Reboot != nil {
			installConfig.Reboot = *kairosConfig.Spec.Install.Reboot
		}
	}

	if installConfig != nil {
		log.Info("Using install configuration", "auto", installConfig.Auto, "device", installConfig.Device, "reboot", installConfig.Reboot)
	} else {
		log.Info("No install configuration provided; install block will be omitted")
	}

	// Get providerID from Machine's infrastructure reference. CAPM3 owns
	// Node.spec.providerID for Metal3 machines and fleet nodes self-discover it
	// after render, so suppress it for both (ADR 0004 / ADR 0008).
	var providerID string
	if !isMetal3Machine(machine) && !isFleetMachine(machine) {
		providerID = r.getProviderID(ctx, log, machine)
	}

	// Control-plane: ask the resolver for the management-endpoint bundle the node
	// needs to push its kubeconfig back without SSH. The resolver may return
	// (nil, nil) as a "disabled" signal, handled the same as "no push block".
	var mgmtEndpoint *ManagementEndpoint
	if supportsManagementEndpoint(machine) && role == "control-plane" && r.MgmtEndpointResolver != nil {
		var err error
		mgmtEndpoint, err = r.MgmtEndpointResolver.Resolve(ctx, kairosConfig, cluster)
		if err != nil {
			return "", err
		}
	}

	// Build the common template data; the distribution table fills the fields that
	// only one distribution's templates read.
	templateData := bootstrap.TemplateData{
		Role:                           role,
		SingleNode:                     singleNode,
		K0sSingleNode:                  k0sSingleNode,
		Hostname:                       hostname,
		UserName:                       userName,
		UserPassword:                   userPassword,
		UserGroups:                     userGroups,
		GitHubUser:                     kairosConfig.Spec.GitHubUser,
		SSHPublicKey:                   kairosConfig.Spec.SSHPublicKey,
		Manifests:                      kairosConfig.Spec.Manifests,
		Files:                          kairosConfig.Spec.Files,
		HostnamePrefix:                 hostnamePrefix,
		DNSServers:                     kairosConfig.Spec.DNSServers,
		PrimaryIP:                      kairosConfig.Spec.PrimaryIP,
		MachineName:                    "",
		ClusterNS:                      "",
		IsKubeVirt:                     isKubevirtMachine(machine),
		Metal3:                         isMetal3Machine(machine),
		IsFleet:                        isFleetMachine(machine),
		Install:                        installConfig,
		ProviderID:                     providerID,
		ControlPlaneLBServiceName:      "",
		ControlPlaneLBServiceNamespace: "",
		ControlPlaneLBEndpoint:         "",
	}
	row.fillTemplateData(&templateData, kairosConfig, serverAddress, workerMat)

	if mgmtEndpoint != nil {
		// One-line conversion keeps internal/bootstrap API-server-unaware.
		// ClusterName and ControlPlaneEndpointHost are stamped from the live
		// Cluster (pure CAPI metadata), not the resolver output.
		templateData.ManagementEndpoint = &bootstrap.ManagementEndpoint{
			APIServer:                 mgmtEndpoint.APIServer,
			Token:                     mgmtEndpoint.Token,
			KubeconfigSecretName:      mgmtEndpoint.KubeconfigSecretName,
			KubeconfigSecretNamespace: mgmtEndpoint.KubeconfigSecretNamespace,
			ClusterName:               cluster.Name,
			ControlPlaneEndpointHost:  cluster.Spec.ControlPlaneEndpoint.Host,
		}
	}
	if machine != nil {
		templateData.MachineName = machine.Name
	}
	if cluster != nil {
		templateData.ClusterNS = cluster.Namespace
		templateData.ControlPlaneLBServiceName = fmt.Sprintf("%s-%s", cluster.Name, controlPlaneLBServiceSuffix)
		templateData.ControlPlaneLBServiceNamespace = cluster.Namespace
	}
	if cluster != nil && isKubevirtMachine(machine) && role == "control-plane" {
		lbEndpoint, err := r.getControlPlaneLBEndpoint(ctx, cluster.Namespace, templateData.ControlPlaneLBServiceName)
		if err != nil {
			return "", fmt.Errorf("failed to get control plane LB endpoint: %w", err)
		}
		if lbEndpoint == "" {
			return "", errLBEndpointNotReady
		}
		templateData.ControlPlaneLBEndpoint = lbEndpoint
	}

	// HA: role / join token / VIP (ADR 0005 Phase 3). No-op on workers (CPR-INV-1).
	if err := r.applyControlPlaneRenderData(ctx, &templateData, kairosConfig, cluster, role); err != nil {
		return "", err
	}

	return bootstrap.Render(dist, templateData)
}

// renderCarriesProviderID replaces the two inline regeneration checks: it reports
// whether cloudConfig embeds providerID and whether it carries the distribution's
// providerID marker. An unknown distribution uses DefaultDistribution's marker, as
// the pre-seam else-branch did.
func renderCarriesProviderID(dist, cloudConfig, providerID string) (hasID, hasMarker bool) {
	hasID = strings.Contains(cloudConfig, providerID)
	marker, ok := bootstrap.ProviderIDMarker(bootstrapv1beta2.EffectiveDistribution(dist))
	if !ok {
		marker, _ = bootstrap.ProviderIDMarker(bootstrapv1beta2.DefaultDistribution)
	}
	hasMarker = strings.Contains(cloudConfig, marker)
	return hasID, hasMarker
}
