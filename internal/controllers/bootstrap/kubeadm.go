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
	"crypto/rand"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/bootstrap"
)

// Bootstrap token Secret layout, see https://kubernetes.io/docs/reference/access-authn-authz/bootstrap-tokens/.
const (
	kubeadmTokenIDKey                   = "token-id"
	kubeadmTokenSecretKey               = "token-secret"
	kubeadmTokenExpirationKey           = "expiration"
	kubeadmTokenUsageAuthenticationKey  = "usage-bootstrap-authentication"
	kubeadmTokenUsageSigningKey         = "usage-bootstrap-signing"
	kubeadmTokenExtraGroupsKey          = "auth-extra-groups"
	kubeadmTokenDefaultNodeGroup        = "system:bootstrappers:kubeadm:default-node-token"
	kubeadmTokenWorkloadSecretPrefix    = "bootstrap-token-"
	kubeadmTokenManagementSecretSuffix  = "kubeadm-bootstrap-token"
	kubeadmTokenIDLength                = 6
	kubeadmTokenSecretLength            = 16
	kubeadmTokenTTL                     = time.Hour
	kubeadmTokenCharset                 = "abcdefghijklmnopqrstuvwxyz0123456789"
	workloadKubeconfigSecretSuffix      = "kubeconfig"
	workloadKubeconfigSecretValueKey    = "value"
	kubeadmTokenUsageEnabled            = "true"
	kubeadmControlPlaneEndpointProtocol = "https://"
)

// ensureKubeadmBootstrapToken returns the management-cluster Secret holding the bootstrap
// token data for the given Machine, creating it on first use. Subsequent calls return the
// same Secret so the worker cloud-config stays stable across reconciles. The Secret carries
// an OwnerReference to the Machine so it is garbage-collected when the Machine is deleted.
// Tokens are valid for one hour and are not renewed here; an expired token will be returned as-is.
func (r *KairosConfigReconciler) ensureKubeadmBootstrapToken(ctx context.Context, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine) (*corev1.Secret, error) {
	if machine == nil {
		return nil, fmt.Errorf("machine is required to generate a kubeadm bootstrap token")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", machine.Name, kubeadmTokenManagementSecretSuffix),
			Namespace: kairosConfig.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if len(secret.Data[kubeadmTokenIDKey]) > 0 && len(secret.Data[kubeadmTokenSecretKey]) > 0 {
			return controllerutil.SetControllerReference(machine, secret, r.Scheme)
		}

		tokenID, err := randomBootstrapTokenString(kubeadmTokenIDLength)
		if err != nil {
			return fmt.Errorf("failed to generate bootstrap token id: %w", err)
		}
		tokenSecret, err := randomBootstrapTokenString(kubeadmTokenSecretLength)
		if err != nil {
			return fmt.Errorf("failed to generate bootstrap token secret: %w", err)
		}

		secret.Type = corev1.SecretTypeBootstrapToken
		secret.Data = map[string][]byte{
			kubeadmTokenIDKey:                  []byte(tokenID),
			kubeadmTokenSecretKey:              []byte(tokenSecret),
			kubeadmTokenExpirationKey:          []byte(time.Now().UTC().Add(kubeadmTokenTTL).Format(time.RFC3339)),
			kubeadmTokenUsageAuthenticationKey: []byte(kubeadmTokenUsageEnabled),
			kubeadmTokenUsageSigningKey:        []byte(kubeadmTokenUsageEnabled),
			kubeadmTokenExtraGroupsKey:         []byte(kubeadmTokenDefaultNodeGroup),
		}
		return controllerutil.SetControllerReference(machine, secret, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("failed to reconcile bootstrap token Secret: %w", err)
	}
	return secret, nil
}

// pushKubeadmBootstrapTokenToWorkloadCluster replicates the bootstrap token Secret into
// kube-system of the workload cluster as bootstrap-token-<id>, using the kubeconfig stored
// in the <cluster>-kubeconfig Secret on the management cluster. The token data (including
// expiration) is copied verbatim so the two sides stay in sync.
func (r *KairosConfigReconciler) pushKubeadmBootstrapTokenToWorkloadCluster(ctx context.Context, cluster *clusterv1.Cluster, mgmtSecret *corev1.Secret) error {
	tokenID := string(mgmtSecret.Data[kubeadmTokenIDKey])
	if tokenID == "" {
		return fmt.Errorf("management bootstrap token Secret %s/%s is missing token-id", mgmtSecret.Namespace, mgmtSecret.Name)
	}

	kubeconfigSecret := &corev1.Secret{}
	key := types.NamespacedName{
		Name:      fmt.Sprintf("%s-%s", cluster.Name, workloadKubeconfigSecretSuffix),
		Namespace: cluster.Namespace,
	}
	if err := r.Get(ctx, key, kubeconfigSecret); err != nil {
		return fmt.Errorf("failed to get workload kubeconfig Secret %s: %w", key, err)
	}
	kubeconfigBytes := kubeconfigSecret.Data[workloadKubeconfigSecretValueKey]
	if len(kubeconfigBytes) == 0 {
		return fmt.Errorf("workload kubeconfig Secret %s has empty 'value' field", key)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return fmt.Errorf("failed to parse workload kubeconfig: %w", err)
	}
	remoteClient, err := client.New(restCfg, client.Options{Scheme: r.Scheme})
	if err != nil {
		return fmt.Errorf("failed to build workload cluster client: %w", err)
	}

	workloadSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadmTokenWorkloadSecretPrefix + tokenID,
			Namespace: metav1.NamespaceSystem,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, remoteClient, workloadSecret, func() error {
		workloadSecret.Type = corev1.SecretTypeBootstrapToken
		workloadSecret.Data = map[string][]byte{}
		maps.Copy(workloadSecret.Data, mgmtSecret.Data)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile bootstrap token Secret in workload cluster: %w", err)
	}
	return nil
}

func (r *KairosConfigReconciler) generateKubeadmCloudConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, role, serverAddress string) (string, error) {
	singleNode := kairosConfig.Spec.SingleNode

	var workerToken string
	if role == "worker" {
		mgmtTokenSecret, err := r.ensureKubeadmBootstrapToken(ctx, kairosConfig, machine)
		if err != nil {
			return "", fmt.Errorf("failed to ensure kubeadm bootstrap token: %w", err)
		}
		if err := r.pushKubeadmBootstrapTokenToWorkloadCluster(ctx, cluster, mgmtTokenSecret); err != nil {
			return "", fmt.Errorf("failed to push kubeadm bootstrap token to workload cluster: %w", err)
		}
		workerToken = fmt.Sprintf("%s.%s", mgmtTokenSecret.Data[kubeadmTokenIDKey], mgmtTokenSecret.Data[kubeadmTokenSecretKey])
	}

	// Set defaults for user configuration
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

	// Set hostname prefix (default to "metal-" if not specified)
	hostnamePrefix := kairosConfig.Spec.HostnamePrefix
	if hostnamePrefix == "" {
		hostnamePrefix = "metal-"
	}

	// Prefer explicit hostname, otherwise use Machine name
	hostname := kairosConfig.Spec.Hostname
	if hostname == "" && machine != nil {
		hostname = machine.Name
	}

	// Set install configuration (with defaults)
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

	// Get providerID from Machine's infrastructure reference
	// This is needed to set the Node's providerID so the Machine controller can match Nodes to Machines
	providerID := r.getProviderID(ctx, log, machine)
	// Build template data
	templateData := bootstrap.TemplateData{
		Role:           role,
		SingleNode:     singleNode,
		Hostname:       hostname,
		UserName:       userName,
		UserPassword:   userPassword,
		UserGroups:     userGroups,
		GitHubUser:     kairosConfig.Spec.GitHubUser,
		SSHPublicKey:   kairosConfig.Spec.SSHPublicKey,
		WorkerToken:    workerToken,
		Manifests:      kairosConfig.Spec.Manifests,
		HostnamePrefix: hostnamePrefix,
		DNSServers:     kairosConfig.Spec.DNSServers,
		PodCIDR:        kairosConfig.Spec.PodCIDR,
		ServiceCIDR:    kairosConfig.Spec.ServiceCIDR,
		PrimaryIP:      kairosConfig.Spec.PrimaryIP,
		IsKubeVirt:     isKubevirtMachine(machine),
		Install:        installConfig,
		ProviderID:     providerID,
		// Workers join through the cluster control-plane endpoint.
		ControlPlaneLBEndpoint: strings.ReplaceAll(serverAddress, kubeadmControlPlaneEndpointProtocol, ""),
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
			return "", err
		}
		if lbEndpoint == "" {
			return "", errLBEndpointNotReady
		}
		templateData.ControlPlaneLBEndpoint = lbEndpoint
	}

	// Render template
	return bootstrap.RenderKubeadmCloudConfig(templateData)
}

// randomBootstrapTokenString returns a random [a-z0-9] string of the given length.
func randomBootstrapTokenString(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = kubeadmTokenCharset[int(b)%len(kubeadmTokenCharset)]
	}
	return string(buf), nil
}
