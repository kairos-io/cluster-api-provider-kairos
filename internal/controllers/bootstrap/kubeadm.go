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
	"k8s.io/client-go/rest"
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
	kubeadmRoleWorker                   = "worker"
	kubeadmDefaultUserName              = "kairos"
	kubeadmDefaultUserGroup             = "admin"
	kubeadmDefaultHostnamePrefix        = "metal-"
	kubeadmDefaultInstallDevice         = "auto"
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

// workloadRESTConfig returns the workload cluster REST config from the <cluster>-kubeconfig Secret.
func (r *KairosConfigReconciler) workloadRESTConfig(ctx context.Context, cluster *clusterv1.Cluster) (*rest.Config, error) {
	kubeconfigSecret := &corev1.Secret{}
	key := types.NamespacedName{
		Name:      fmt.Sprintf("%s-%s", cluster.Name, workloadKubeconfigSecretSuffix),
		Namespace: cluster.Namespace,
	}
	if err := r.Get(ctx, key, kubeconfigSecret); err != nil {
		return nil, fmt.Errorf("failed to get workload kubeconfig Secret %s: %w", key, err)
	}
	kubeconfigBytes := kubeconfigSecret.Data[workloadKubeconfigSecretValueKey]
	if len(kubeconfigBytes) == 0 {
		return nil, fmt.Errorf("workload kubeconfig Secret %s has empty 'value' field", key)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse workload kubeconfig: %w", err)
	}
	if len(restCfg.CAData) == 0 {
		return nil, fmt.Errorf("workload kubeconfig Secret %s has no certificate-authority-data", key)
	}
	return restCfg, nil
}

// pushKubeadmBootstrapTokenToWorkloadCluster replicates the bootstrap token Secret into
// kube-system of the workload cluster as bootstrap-token-<id>. The token data (including
// expiration) is copied verbatim so the two sides stay in sync.
func (r *KairosConfigReconciler) pushKubeadmBootstrapTokenToWorkloadCluster(ctx context.Context, restCfg *rest.Config, mgmtSecret *corev1.Secret) error {
	tokenID := string(mgmtSecret.Data[kubeadmTokenIDKey])
	if tokenID == "" {
		return fmt.Errorf("management bootstrap token Secret %s/%s is missing token-id", mgmtSecret.Namespace, mgmtSecret.Name)
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

// generateKubeadmCloudConfig renders a provider-kubernetes worker cloud-config that joins the workload
// cluster with a per-Machine bootstrap token pinned to the workload cluster CA.
func (r *KairosConfigReconciler) generateKubeadmCloudConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, role, serverAddress string) (string, error) {
	if role != kubeadmRoleWorker {
		return "", fmt.Errorf("kubeadm distribution only supports the %s role, got %q", kubeadmRoleWorker, role)
	}
	if machine == nil || cluster == nil {
		return "", fmt.Errorf("kubeadm worker cloud-config requires both a Machine and a Cluster")
	}
	if machine.Spec.Version == "" {
		return "", fmt.Errorf("machine %s/%s has no spec.version", machine.Namespace, machine.Name)
	}

	restCfg, err := r.workloadRESTConfig(ctx, cluster)
	if err != nil {
		return "", err
	}
	mgmtTokenSecret, err := r.ensureKubeadmBootstrapToken(ctx, kairosConfig, machine)
	if err != nil {
		return "", fmt.Errorf("failed to ensure kubeadm bootstrap token: %w", err)
	}
	if err := r.pushKubeadmBootstrapTokenToWorkloadCluster(ctx, restCfg, mgmtTokenSecret); err != nil {
		return "", fmt.Errorf("failed to push kubeadm bootstrap token to workload cluster: %w", err)
	}

	// Get providerID from Machine's infrastructure reference so the Node registers with it.
	providerID := r.getProviderID(ctx, log, machine)
	kubeadmCluster, err := bootstrap.NewKubeadmWorkerCluster(bootstrap.KubeadmWorkerParams{
		ClusterToken:      string(cluster.UID),
		ControlPlaneHost:  strings.TrimPrefix(serverAddress, kubeadmControlPlaneEndpointProtocol),
		CACertPEM:         string(restCfg.CAData),
		KubernetesVersion: machine.Spec.Version,
		BootstrapToken:    fmt.Sprintf("%s.%s", mgmtTokenSecret.Data[kubeadmTokenIDKey], mgmtTokenSecret.Data[kubeadmTokenSecretKey]),
		ProviderID:        providerID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to build kubeadm cluster block: %w", err)
	}

	userPassword, err := r.resolveUserPassword(ctx, kairosConfig)
	if err != nil {
		return "", err
	}

	templateData := bootstrap.TemplateData{
		Role:           role,
		Hostname:       kairosConfig.Spec.Hostname,
		HostnamePrefix: kairosConfig.Spec.HostnamePrefix,
		UserName:       kairosConfig.Spec.UserName,
		UserPassword:   userPassword,
		UserGroups:     kairosConfig.Spec.UserGroups,
		GitHubUser:     kairosConfig.Spec.GitHubUser,
		SSHPublicKey:   kairosConfig.Spec.SSHPublicKey,
		DNSServers:     kairosConfig.Spec.DNSServers,
		Install:        kubeadmInstallConfig(kairosConfig.Spec.Install),
		ProviderID:     providerID,
		KubeadmCluster: kubeadmCluster,
	}
	applyKubeadmTemplateDefaults(&templateData, machine)

	return bootstrap.RenderKubeadmCloudConfig(templateData)
}

// applyKubeadmTemplateDefaults fills the user and hostname defaults the other distributions also use.
func applyKubeadmTemplateDefaults(data *bootstrap.TemplateData, machine *clusterv1.Machine) {
	if data.UserName == "" {
		data.UserName = kubeadmDefaultUserName
	}
	if len(data.UserGroups) == 0 {
		data.UserGroups = []string{kubeadmDefaultUserGroup}
	}
	if data.HostnamePrefix == "" {
		data.HostnamePrefix = kubeadmDefaultHostnamePrefix
	}
	if data.Hostname == "" {
		data.Hostname = machine.Name
	}
}

// kubeadmInstallConfig converts the KairosConfig install spec, defaulting to an automatic install with reboot.
func kubeadmInstallConfig(spec *bootstrapv1beta2.InstallConfig) *bootstrap.InstallConfig {
	if spec == nil {
		return nil
	}
	cfg := &bootstrap.InstallConfig{Auto: true, Device: kubeadmDefaultInstallDevice, Reboot: true}
	if spec.Auto != nil {
		cfg.Auto = *spec.Auto
	}
	if spec.Device != "" {
		cfg.Device = spec.Device
	}
	if spec.Reboot != nil {
		cfg.Reboot = *spec.Reboot
	}
	return cfg
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
