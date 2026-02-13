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
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1beta2 "github.com/kairos-io/kairos-capi/api/bootstrap/v1beta2"
	"github.com/kairos-io/kairos-capi/internal/bootstrap"
)

const controlPlaneLBServiceSuffix = "control-plane-lb"

var errLBEndpointNotReady = errors.New("control plane load balancer endpoint not ready")
var errK3sTokenNotReady = errors.New("k3s token secret not ready")

// KairosConfigReconciler reconciles a KairosConfig object
type KairosConfigReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RESTConfig *rest.Config
}

//+kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines/status;clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspheremachines,verbs=get;list;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspheremachines/status,verbs=get
//+kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets;events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts;serviceaccounts/token,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;list;update;patch;watch
//+kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=get;list;patch;update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;patch;update

// Reconcile is part of the main kubernetes reconciliation loop
func (r *KairosConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the KairosConfig instance
	kairosConfig := &bootstrapv1beta2.KairosConfig{}
	if err := r.Get(ctx, req.NamespacedName, kairosConfig); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !kairosConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, kairosConfig)
	}

	// Add finalizer if needed
	if !controllerutil.ContainsFinalizer(kairosConfig, bootstrapv1beta2.KairosConfigFinalizer) {
		controllerutil.AddFinalizer(kairosConfig, bootstrapv1beta2.KairosConfigFinalizer)
		if err := r.Update(ctx, kairosConfig); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if paused
	if kairosConfig.Spec.Pause {
		log.Info("KairosConfig is paused, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Find the owning Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, kairosConfig.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to get owner machine")
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Machine Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	// Find the owning Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to get cluster from machine metadata")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Cluster is not available yet")
		return ctrl.Result{}, nil
	}

	// Initialize patch helper
	helper, err := patch.NewHelper(kairosConfig, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always update observedGeneration
	kairosConfig.Status.ObservedGeneration = kairosConfig.Generation

	// Reconcile bootstrap data
	result, err := r.reconcileBootstrapData(ctx, log, kairosConfig, machine, cluster)
	if err != nil {
		// Mark conditions as false on error
		// Use "%s" as format string and pass error as argument to satisfy linter
		conditions.MarkFalse(kairosConfig, clusterv1.ReadyCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		conditions.MarkFalse(kairosConfig, bootstrapv1beta2.BootstrapReadyCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		conditions.MarkFalse(kairosConfig, bootstrapv1beta2.DataSecretAvailableCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())

		kairosConfig.Status.FailureReason = bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason
		kairosConfig.Status.FailureMessage = err.Error()
		kairosConfig.Status.Ready = false

		return ctrl.Result{}, helper.Patch(ctx, kairosConfig)
	}

	// If reconcileBootstrapData requested a requeue (e.g., waiting for providerID), return it
	if result.Requeue || result.RequeueAfter > 0 {
		return result, helper.Patch(ctx, kairosConfig)
	}

	// Mark conditions as true on success
	conditions.MarkTrue(kairosConfig, clusterv1.ReadyCondition)
	conditions.MarkTrue(kairosConfig, bootstrapv1beta2.BootstrapReadyCondition)
	conditions.MarkTrue(kairosConfig, bootstrapv1beta2.DataSecretAvailableCondition)

	// Clear failure fields
	kairosConfig.Status.FailureReason = ""
	kairosConfig.Status.FailureMessage = ""

	// Update status
	return ctrl.Result{}, helper.Patch(ctx, kairosConfig)
}

func (r *KairosConfigReconciler) reconcileBootstrapData(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	// Get providerID - it may not be available initially (before VM is created)
	// We allow bootstrap secret creation without providerID initially, then regenerate when providerID becomes available
	currentProviderID := r.getProviderID(ctx, log, machine)

	// For VSphere: Only wait for providerID if VSphereMachine is Ready (VM already provisioned)
	// If VSphereMachine is not Ready yet, allow secret creation so VM can be provisioned
	// This avoids circular dependency: VM needs bootstrap secret to be created, but providerID is set after VM creation
	if machine != nil && machine.Spec.InfrastructureRef.Kind == "VSphereMachine" && currentProviderID == "" {
		vsphereMachine := &unstructured.Unstructured{}
		vsphereMachine.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "infrastructure.cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "VSphereMachine",
		})
		vsphereMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereMachineKey, vsphereMachine); err == nil {
			// Check if VSphereMachine is Ready (VM provisioned)
			// Look for Ready condition in status.conditions array
			conditions, found, _ := unstructured.NestedSlice(vsphereMachine.Object, "status", "conditions")
			isReady := false
			if found {
				for _, cond := range conditions {
					condMap, ok := cond.(map[string]interface{})
					if ok {
						condType, _ := condMap["type"].(string)
						condStatus, _ := condMap["status"].(string)
						if condType == "Ready" && condStatus == "True" {
							isReady = true
							break
						}
					}
				}
			}

			// Only wait for providerID if VM is already Ready (provisioned)
			// If VM is not Ready yet, proceed with secret creation (VM needs bootstrap secret to be created first)
			if isReady {
				log.V(4).Info("VSphereMachine is Ready but providerID not yet set, waiting briefly for CAPV to set it",
					"machine", machine.Name,
					"vsphereMachine", vsphereMachineKey.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// If VM is not Ready yet, proceed with secret creation - this allows VM to be provisioned
			log.V(5).Info("VSphereMachine not Ready yet, proceeding with bootstrap secret creation",
				"machine", machine.Name,
				"vsphereMachine", vsphereMachineKey.Name)
		}
	}

	// For CAPK: Only wait for providerID if KubevirtMachine is Ready (VM already provisioned)
	// If KubevirtMachine is not Ready yet, allow secret creation so VM can be provisioned
	if machine != nil && (machine.Spec.InfrastructureRef.Kind == "KubevirtMachine" || machine.Spec.InfrastructureRef.Kind == "KubeVirtMachine") && currentProviderID == "" {
		kubevirtMachine := &unstructured.Unstructured{}
		kubevirtMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		kubevirtMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, kubevirtMachineKey, kubevirtMachine); err == nil {
			isReady := false
			if ready, found, _ := unstructured.NestedBool(kubevirtMachine.Object, "status", "ready"); found && ready {
				isReady = true
			}
			if !isReady {
				conditions, found, _ := unstructured.NestedSlice(kubevirtMachine.Object, "status", "conditions")
				if found {
					for _, cond := range conditions {
						condMap, ok := cond.(map[string]interface{})
						if ok {
							condType, _ := condMap["type"].(string)
							condStatus, _ := condMap["status"].(string)
							if condType == "Ready" && condStatus == "True" {
								isReady = true
								break
							}
						}
					}
				}
			}

			if isReady {
				log.V(4).Info("KubevirtMachine is Ready but providerID not yet set, waiting briefly for CAPK to set it",
					"machine", machine.Name,
					"kubevirtMachine", kubevirtMachineKey.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			log.V(5).Info("KubevirtMachine not Ready yet, proceeding with bootstrap secret creation",
				"machine", machine.Name,
				"kubevirtMachine", kubevirtMachineKey.Name)
		}
	}

	// If Machine has a bootstrap dataSecretName that differs from status, align to Machine to avoid duplicates.
	if machine != nil && machine.Spec.Bootstrap.DataSecretName != nil && *machine.Spec.Bootstrap.DataSecretName != "" &&
		kairosConfig.Status.DataSecretName != nil && *kairosConfig.Status.DataSecretName != "" &&
		*machine.Spec.Bootstrap.DataSecretName != *kairosConfig.Status.DataSecretName {
		oldSecretName := *kairosConfig.Status.DataSecretName
		newSecretName := *machine.Spec.Bootstrap.DataSecretName
		log.Info("Bootstrap secret name mismatch; aligning to Machine",
			"oldSecret", oldSecretName,
			"newSecret", newSecretName,
			"machine", machine.Name)

		oldSecret := &corev1.Secret{}
		oldKey := types.NamespacedName{Name: oldSecretName, Namespace: kairosConfig.Namespace}
		if err := r.Get(ctx, oldKey, oldSecret); err == nil {
			_ = r.Delete(ctx, oldSecret)
		}
		oldUserdata := &corev1.Secret{}
		oldUserdataKey := types.NamespacedName{Name: fmt.Sprintf("%s-userdata", oldSecretName), Namespace: kairosConfig.Namespace}
		if err := r.Get(ctx, oldUserdataKey, oldUserdata); err == nil {
			_ = r.Delete(ctx, oldUserdata)
		}

		kairosConfig.Status.DataSecretName = nil
	}

	// If dataSecretName is already set, verify the secret exists and check if regeneration is needed
	if kairosConfig.Status.DataSecretName != nil {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      *kairosConfig.Status.DataSecretName,
			Namespace: kairosConfig.Namespace,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			if apierrors.IsNotFound(err) {
				// Secret was deleted, regenerate
				log.Info("Bootstrap secret was deleted, regenerating", "secret", *kairosConfig.Status.DataSecretName)
				kairosConfig.Status.DataSecretName = nil
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to get bootstrap secret: %w", err)
			}
		} else {
			// Secret exists, check if we need to regenerate it due to providerID availability
			needsRegeneration := false
			currentProviderID := r.getProviderID(ctx, log, machine)

			if currentProviderID != "" {
				// Machine has providerID, check if the secret contains it
				secretData, ok := secret.Data["value"]
				if !ok {
					log.Info("Bootstrap secret missing data, regenerating", "secret", *kairosConfig.Status.DataSecretName)
					needsRegeneration = true
				} else {
					// Kubernetes Secrets are stored base64-encoded in etcd, but client-go
					// already decodes them into Secret.Data. Treat it as plain text.
					cloudConfigStr := string(secretData)
					// Check if providerID is present in the script
					hasProviderIDInSecret := strings.Contains(cloudConfigStr, currentProviderID)

					distribution := kairosConfig.Spec.Distribution
					if distribution == "" {
						distribution = "k0s"
					}
					// Check if there's a post-bootstrap service (indicating providerID was included)
					// If Machine has providerID but secret has no service, we need to regenerate
					hasPostBootstrapService := strings.Contains(cloudConfigStr, "kairos-k0s-post-bootstrap.service")
					if distribution == "k3s" {
						hasPostBootstrapService = strings.Contains(cloudConfigStr, "kairos-k3s-post-bootstrap.service")
					}
					// Ensure SSH enable stage exists (regression guard for CAPV access)
					hasSSHEnableStage := strings.Contains(cloudConfigStr, "systemctl enable --now sshd") ||
						strings.Contains(cloudConfigStr, "systemctl enable --now ssh")
					hasSSHPassAuth := strings.Contains(cloudConfigStr, "PasswordAuthentication yes")

					if currentProviderID != "" && (!hasProviderIDInSecret || !hasPostBootstrapService) {
						log.Info("Bootstrap secret missing providerID in post-bootstrap service, regenerating to include it",
							"secret", *kairosConfig.Status.DataSecretName,
							"providerID", currentProviderID,
							"hasProviderIDInSecret", hasProviderIDInSecret,
							"hasPostBootstrapService", hasPostBootstrapService)
						needsRegeneration = true
					}
					if !hasSSHEnableStage || !hasSSHPassAuth {
						log.Info("Bootstrap secret missing SSH settings, regenerating",
							"secret", *kairosConfig.Status.DataSecretName)
						needsRegeneration = true
					}
				}
			}

			if needsRegeneration {
				// Keep the existing secret name and regenerate its contents.
				// The Machine's bootstrap dataSecretName is immutable, so we must not change it.
				log.Info("Bootstrap secret needs regeneration; will update existing secret",
					"secret", *kairosConfig.Status.DataSecretName)
			} else {
				// Secret exists and is up-to-date, verify it's ready
				log.V(4).Info("Bootstrap data already generated and up-to-date", "secret", *kairosConfig.Status.DataSecretName)
				kairosConfig.Status.Ready = true
				// Ensure initialization.dataSecretCreated is set
				if kairosConfig.Status.Initialization == nil {
					kairosConfig.Status.Initialization = &bootstrapv1beta2.KairosConfigInitialization{}
				}
				kairosConfig.Status.Initialization.DataSecretCreated = true

				if isKubevirtMachine(machine) {
					updated, found, err := r.sanitizeCapkUserdataSecret(ctx, log, kairosConfig, machine)
					if err != nil {
						return ctrl.Result{}, err
					}
					if !found {
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
					if updated {
						log.Info("Sanitized CAPK userdata secret", "secret", *kairosConfig.Status.DataSecretName)
					}
				}

				return ctrl.Result{}, nil
			}
		}
	}

	// Generate Kairos cloud-config
	cloudConfig, err := r.generateCloudConfig(ctx, log, kairosConfig, machine, cluster)
	if err != nil {
		if errors.Is(err, errLBEndpointNotReady) {
			log.Info("Waiting for control plane LoadBalancer endpoint before generating cloud-config")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if errors.Is(err, errK3sTokenNotReady) {
			log.Info("Waiting for k3s token secret before generating cloud-config")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to generate cloud-config: %w", err)
	}

	// Store the cloud-config as plain text in the secret
	// Kubernetes will automatically base64 encode it when storing in etcd
	// CAPV will read it, base64 decode it (removing Kubernetes encoding), and get plain text
	// CAPV will then pass it to VMware guestinfo properties as userdata (and possibly vendordata)
	// Note: There is a known issue where CAPV may set vendordata without setting guestinfo.vendordata.encoding,
	// which causes Kairos to log "VMWare: Failed to get vendordata: Unknown encoding". However, userdata works
	// correctly, so this error is non-blocking and the cluster will function properly.
	// Do NOT base64 encode it ourselves - let CAPV handle the encoding

	// Create Secret with bootstrap data
	secretName := ""
	if machine != nil && machine.Spec.Bootstrap.DataSecretName != nil && *machine.Spec.Bootstrap.DataSecretName != "" {
		secretName = *machine.Spec.Bootstrap.DataSecretName
	} else if kairosConfig.Status.DataSecretName != nil && *kairosConfig.Status.DataSecretName != "" {
		secretName = *kairosConfig.Status.DataSecretName
	} else {
		randomSuffix, err := randomString(6)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to generate random string: %w", err)
		}
		secretName = fmt.Sprintf("%s-%s", kairosConfig.Name, randomSuffix)
	}

	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: kairosConfig.Namespace,
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: kairosConfig.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kairosConfig.APIVersion,
					Kind:       kairosConfig.Kind,
					Name:       kairosConfig.Name,
					UID:        kairosConfig.UID,
					Controller: func() *bool { b := true; return &b }(),
				},
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{
			"value": []byte(cloudConfig),
		},
	}

	// Create or update the secret in-place to preserve the name referenced by Machine
	existingSecret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, existingSecret); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, secret); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{}, err
		}
	} else {
		existingSecret.Type = secret.Type
		existingSecret.Labels = secret.Labels
		existingSecret.OwnerReferences = secret.OwnerReferences
		existingSecret.Data = secret.Data
		if err := r.Update(ctx, existingSecret); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Update status with dataSecretName
	kairosConfig.Status.DataSecretName = &secretName

	// Mark secret as Ready - providerID will be included if available, otherwise it will be regenerated later
	// We allow the secret to be Ready even without providerID initially, so VM can be created
	// When providerID becomes available (via VSphereMachine watch), the secret will be regenerated
	if currentProviderID != "" {
		// Verify providerID is included in the cloud-config
		// cloudConfig is plain text, no need to decode
		hasProviderIDInSecret := strings.Contains(cloudConfig, currentProviderID)
		distribution := kairosConfig.Spec.Distribution
		if distribution == "" {
			distribution = "k0s"
		}
		// Check for the systemd service that sets providerID (runs after k3s/k0s service starts)
		hasPostBootstrapService := strings.Contains(cloudConfig, "kairos-k0s-post-bootstrap.service")
		if distribution == "k3s" {
			hasPostBootstrapService = strings.Contains(cloudConfig, "kairos-k3s-post-bootstrap.service")
		}

		if hasProviderIDInSecret && hasPostBootstrapService {
			kairosConfig.Status.Ready = true
			log.Info("Bootstrap data secret created with providerID", "secret", secretName, "providerID", currentProviderID)
		} else {
			// ProviderID should be included but wasn't - regenerate
			log.Info("Bootstrap secret created but providerID not properly included, will regenerate",
				"secret", secretName,
				"providerID", currentProviderID,
				"hasProviderID", hasProviderIDInSecret,
				"hasPostBootstrapService", hasPostBootstrapService)
			kairosConfig.Status.Ready = false
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	} else {
		// No providerID available yet - mark as Ready so VM can be created
		// When providerID becomes available (via VSphereMachine watch), secret will be regenerated
		kairosConfig.Status.Ready = true
		log.Info("Bootstrap secret created without providerID (will be regenerated when providerID becomes available)",
			"secret", secretName)
	}

	// Set initialization.dataSecretCreated as required by Cluster API contract
	// This field is used by the Machine controller to determine when bootstrap data is ready
	if kairosConfig.Status.Initialization == nil {
		kairosConfig.Status.Initialization = &bootstrapv1beta2.KairosConfigInitialization{}
	}
	kairosConfig.Status.Initialization.DataSecretCreated = true

	if isKubevirtMachine(machine) {
		updated, found, err := r.sanitizeCapkUserdataSecret(ctx, log, kairosConfig, machine)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !found {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if updated {
			log.Info("Sanitized CAPK userdata secret", "secret", secretName)
		}
	}

	return ctrl.Result{}, nil
}

func isKubevirtMachine(machine *clusterv1.Machine) bool {
	if machine == nil {
		return false
	}
	return machine.Spec.InfrastructureRef.Kind == "KubevirtMachine" || machine.Spec.InfrastructureRef.Kind == "KubeVirtMachine"
}

func (r *KairosConfigReconciler) sanitizeCapkUserdataSecret(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine) (bool, bool, error) {
	secretName := ""
	if machine != nil && machine.Spec.Bootstrap.DataSecretName != nil && *machine.Spec.Bootstrap.DataSecretName != "" {
		secretName = *machine.Spec.Bootstrap.DataSecretName
	} else if kairosConfig.Status.DataSecretName != nil && *kairosConfig.Status.DataSecretName != "" {
		secretName = *kairosConfig.Status.DataSecretName
	}
	if secretName == "" {
		return false, false, nil
	}

	userdataSecretName := fmt.Sprintf("%s-userdata", secretName)
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      userdataSecretName,
		Namespace: kairosConfig.Namespace,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}

	userdata, ok := secret.Data["userdata"]
	if !ok || len(userdata) == 0 {
		return false, true, nil
	}

	updated, changed := sanitizeCapkUserdata(string(userdata))
	if !changed {
		return false, true, nil
	}

	secret.Data["userdata"] = []byte(updated)
	if err := r.Update(ctx, secret); err != nil {
		if apierrors.IsConflict(err) {
			log.V(4).Info("CAPK userdata secret update conflicted; will retry on next reconcile", "secret", userdataSecretName)
			return false, true, nil
		}
		return false, true, err
	}

	log.V(4).Info("Updated CAPK userdata secret", "secret", userdataSecretName)
	return true, true, nil
}

func sanitizeCapkUserdata(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	updated := make([]string, 0, len(lines))
	currentUser := ""
	inSSHKeys := false
	sshIndent := 0
	changed := false
	expectGroupsList := false
	groupsIndent := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- name:") {
			currentUser = strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))
			inSSHKeys = false
			expectGroupsList = false
		}

		if expectGroupsList {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if indent > groupsIndent {
				changed = true
				continue
			}
			expectGroupsList = false
		}

		if inSSHKeys {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if indent > sshIndent {
				if strings.HasPrefix(strings.TrimSpace(trimmed), "-") {
					keyValue := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
					if strings.HasPrefix(keyValue, "\"") && strings.HasSuffix(keyValue, "\"") {
						keyValue = strings.TrimSuffix(strings.TrimPrefix(keyValue, "\""), "\"")
					}
					if keyValue != "" {
						line = strings.Repeat(" ", indent) + "- \"" + keyValue + "\""
						changed = true
					}
				}
				updated = append(updated, line)
				continue
			}
			inSSHKeys = false
		}

		if currentUser == "capk" {
			if strings.HasPrefix(trimmed, "sudo:") {
				changed = true
				continue
			}
			if strings.HasPrefix(trimmed, "ssh_authorized_keys:") {
				inSSHKeys = true
				sshIndent = len(line) - len(strings.TrimLeft(line, " "))
				updated = append(updated, line)
				continue
			}
			if trimmed == "groups: users, admin" {
				indent := len(line) - len(strings.TrimLeft(line, " "))
				line = strings.Repeat(" ", indent) + "groups: [users, admin]"
				changed = true
			} else if trimmed == "groups:" {
				groupsIndent = len(line) - len(strings.TrimLeft(line, " "))
				line = strings.Repeat(" ", groupsIndent) + "groups: [users, admin]"
				expectGroupsList = true
				changed = true
			}
		}

		updated = append(updated, line)
	}

	return strings.Join(updated, "\n"), changed
}

func (r *KairosConfigReconciler) generateCloudConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster) (string, error) {
	// Determine role
	role := kairosConfig.Spec.Role
	if role == "" {
		// Infer from machine labels
		if util.IsControlPlaneMachine(machine) {
			role = "control-plane"
		} else {
			role = "worker"
		}
	}

	// Determine distribution
	distribution := kairosConfig.Spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}

	// Get cluster information
	serverAddress := kairosConfig.Spec.ServerAddress
	if serverAddress == "" && cluster.Spec.ControlPlaneEndpoint.IsValid() {
		serverAddress = fmt.Sprintf("https://%s:%d", cluster.Spec.ControlPlaneEndpoint.Host, cluster.Spec.ControlPlaneEndpoint.Port)
	}

	// Generate cloud-config based on distribution
	switch distribution {
	case "k0s":
		return r.generateK0sCloudConfig(ctx, log, kairosConfig, machine, cluster, role, serverAddress)
	case "k3s":
		return r.generateK3sCloudConfig(ctx, log, kairosConfig, machine, cluster, role, serverAddress)
	default:
		return "", fmt.Errorf("unsupported distribution: %s", distribution)
	}
}

func (r *KairosConfigReconciler) generateK0sCloudConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, role, serverAddress string) (string, error) {
	// Determine single-node mode
	// Single-node is determined by:
	// 1. Explicit flag in KairosConfig.spec.singleNode
	// 2. Or if this is a control-plane and we can check the owning KairosControlPlane
	singleNode := kairosConfig.Spec.SingleNode
	if !singleNode && role == "control-plane" && machine != nil {
		// Try to find the owning KairosControlPlane to check replicas
		ownerRef := metav1.GetControllerOf(machine)
		if ownerRef != nil && ownerRef.Kind == "KairosControlPlane" {
			// For now, we rely on the SingleNode flag in spec
			// In the future, we could fetch the KCP and check spec.replicas == 1
			log.V(4).Info("Control plane node, single-node mode determined from spec", "singleNode", singleNode)
		}
	}

	// Get worker token if needed (for worker nodes)
	// Precedence: WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token
	// TODO: Add validating webhook to enforce worker token requirement at API level
	var workerToken string
	if role == "worker" {
		// Try WorkerTokenSecretRef first (most secure)
		if kairosConfig.Spec.WorkerTokenSecretRef != nil {
			secretKey := types.NamespacedName{
				Namespace: kairosConfig.Namespace,
				Name:      kairosConfig.Spec.WorkerTokenSecretRef.Name,
			}
			// Use specified namespace or fall back to KairosConfig namespace
			if kairosConfig.Spec.WorkerTokenSecretRef.Namespace != "" {
				secretKey.Namespace = kairosConfig.Spec.WorkerTokenSecretRef.Namespace
			}

			secret := &corev1.Secret{}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				return "", fmt.Errorf("failed to get worker token secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
			}

			// Use specified key or default to "token"
			key := kairosConfig.Spec.WorkerTokenSecretRef.Key
			if key == "" {
				key = "token"
			}

			if tokenData, ok := secret.Data[key]; ok {
				workerToken = string(tokenData)
			} else {
				return "", fmt.Errorf("worker token secret %s/%s does not contain key '%s'", secretKey.Namespace, secretKey.Name, key)
			}
		} else if kairosConfig.Spec.WorkerToken != "" {
			// Fall back to inline WorkerToken
			workerToken = kairosConfig.Spec.WorkerToken
		} else if kairosConfig.Spec.TokenSecretRef != nil {
			// Fall back to legacy TokenSecretRef
			secret := &corev1.Secret{}
			secretKey := types.NamespacedName{
				Namespace: cluster.Namespace,
				Name:      kairosConfig.Spec.TokenSecretRef.Name,
			}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				return "", fmt.Errorf("failed to get token secret: %w", err)
			}
			// Try common token keys
			if tokenData, ok := secret.Data["token"]; ok {
				workerToken = string(tokenData)
			} else if tokenData, ok := secret.Data["value"]; ok {
				workerToken = string(tokenData)
			} else {
				return "", fmt.Errorf("token secret does not contain 'token' or 'value' key")
			}
		} else if kairosConfig.Spec.Token != "" {
			// Fall back to legacy Token
			workerToken = kairosConfig.Spec.Token
		}

		// Validate worker token is present
		if workerToken == "" {
			return "", fmt.Errorf("worker token is required for worker nodes: either WorkerTokenSecretRef, WorkerToken, TokenSecretRef, or Token must be set")
		}
	}

	// Set defaults for user configuration
	userName := kairosConfig.Spec.UserName
	if userName == "" {
		userName = "kairos"
	}
	userPassword := kairosConfig.Spec.UserPassword
	if userPassword == "" {
		userPassword = "kairos"
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

	var kubeconfigPush *kubeconfigPushConfig
	if isKubevirtMachine(machine) && role == "control-plane" {
		var err error
		kubeconfigPush, err = r.ensureKubeconfigPushConfig(ctx, log, kairosConfig, cluster)
		if err != nil {
			return "", err
		}
	}

	// Build template data
	templateData := bootstrap.TemplateData{
		Role:                                role,
		SingleNode:                          singleNode,
		Hostname:                            hostname,
		UserName:                            userName,
		UserPassword:                        userPassword,
		UserGroups:                          userGroups,
		GitHubUser:                          kairosConfig.Spec.GitHubUser,
		SSHPublicKey:                        kairosConfig.Spec.SSHPublicKey,
		WorkerToken:                         workerToken,
		Manifests:                           kairosConfig.Spec.Manifests,
		HostnamePrefix:                      hostnamePrefix,
		DNSServers:                          kairosConfig.Spec.DNSServers,
		PodCIDR:                             kairosConfig.Spec.PodCIDR,
		ServiceCIDR:                         kairosConfig.Spec.ServiceCIDR,
		PrimaryIP:                           kairosConfig.Spec.PrimaryIP,
		MachineName:                         "",
		ClusterNS:                           "",
		IsKubeVirt:                          isKubevirtMachine(machine),
		Install:                             installConfig,
		ProviderID:                          providerID,
		ControlPlaneLBServiceName:           "",
		ControlPlaneLBServiceNamespace:      "",
		ControlPlaneLBEndpoint:              "",
		ManagementKubeconfigToken:           "",
		ManagementKubeconfigSecretName:      "",
		ManagementKubeconfigSecretNamespace: "",
		ManagementAPIServer:                 "",
	}
	if kubeconfigPush != nil {
		templateData.ManagementKubeconfigToken = kubeconfigPush.Token
		templateData.ManagementKubeconfigSecretName = kubeconfigPush.SecretName
		templateData.ManagementKubeconfigSecretNamespace = kubeconfigPush.SecretNamespace
		templateData.ManagementAPIServer = kubeconfigPush.APIServer
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
	return bootstrap.RenderK0sCloudConfig(templateData)
}

func (r *KairosConfigReconciler) generateK3sCloudConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, role, serverAddress string) (string, error) {
	// Determine single-node mode
	singleNode := kairosConfig.Spec.SingleNode
	if !singleNode && role == "control-plane" && machine != nil {
		ownerRef := metav1.GetControllerOf(machine)
		if ownerRef != nil && ownerRef.Kind == "KairosControlPlane" {
			log.V(4).Info("Control plane node, single-node mode determined from spec", "singleNode", singleNode)
		}
	}

	// Resolve k3s token if needed (for worker nodes)
	// Precedence: K3sTokenSecretRef > K3sToken > WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token
	var k3sToken string
	if role == "worker" {
		if kairosConfig.Spec.K3sTokenSecretRef != nil {
			secretKey := types.NamespacedName{
				Namespace: kairosConfig.Namespace,
				Name:      kairosConfig.Spec.K3sTokenSecretRef.Name,
			}
			if kairosConfig.Spec.K3sTokenSecretRef.Namespace != "" {
				secretKey.Namespace = kairosConfig.Spec.K3sTokenSecretRef.Namespace
			}

			secret := &corev1.Secret{}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				if apierrors.IsNotFound(err) {
					return "", errK3sTokenNotReady
				}
				return "", fmt.Errorf("failed to get k3s token secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
			}

			key := kairosConfig.Spec.K3sTokenSecretRef.Key
			if key == "" {
				key = "token"
			}

			if tokenData, ok := secret.Data[key]; ok {
				k3sToken = string(tokenData)
			} else {
				return "", fmt.Errorf("k3s token secret %s/%s does not contain key '%s'", secretKey.Namespace, secretKey.Name, key)
			}
		} else if kairosConfig.Spec.K3sToken != "" {
			k3sToken = kairosConfig.Spec.K3sToken
		} else if kairosConfig.Spec.WorkerTokenSecretRef != nil {
			secretKey := types.NamespacedName{
				Namespace: kairosConfig.Namespace,
				Name:      kairosConfig.Spec.WorkerTokenSecretRef.Name,
			}
			if kairosConfig.Spec.WorkerTokenSecretRef.Namespace != "" {
				secretKey.Namespace = kairosConfig.Spec.WorkerTokenSecretRef.Namespace
			}

			secret := &corev1.Secret{}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				if apierrors.IsNotFound(err) {
					return "", errK3sTokenNotReady
				}
				return "", fmt.Errorf("failed to get worker token secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
			}

			key := kairosConfig.Spec.WorkerTokenSecretRef.Key
			if key == "" {
				key = "token"
			}

			if tokenData, ok := secret.Data[key]; ok {
				k3sToken = string(tokenData)
			} else {
				return "", fmt.Errorf("worker token secret %s/%s does not contain key '%s'", secretKey.Namespace, secretKey.Name, key)
			}
		} else if kairosConfig.Spec.WorkerToken != "" {
			k3sToken = kairosConfig.Spec.WorkerToken
		} else if kairosConfig.Spec.TokenSecretRef != nil {
			secretKey := types.NamespacedName{
				Namespace: cluster.Namespace,
				Name:      kairosConfig.Spec.TokenSecretRef.Name,
			}
			secret := &corev1.Secret{}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				if apierrors.IsNotFound(err) {
					return "", errK3sTokenNotReady
				}
				return "", fmt.Errorf("failed to get token secret: %w", err)
			}
			if tokenData, ok := secret.Data["token"]; ok {
				k3sToken = string(tokenData)
			} else if tokenData, ok := secret.Data["value"]; ok {
				k3sToken = string(tokenData)
			} else {
				return "", fmt.Errorf("token secret does not contain 'token' or 'value' key")
			}
		} else if kairosConfig.Spec.Token != "" {
			k3sToken = kairosConfig.Spec.Token
		}

		if k3sToken == "" {
			return "", fmt.Errorf("k3s worker requires a join token: set k3sTokenSecretRef, k3sToken, workerTokenSecretRef, workerToken, tokenSecretRef, or token")
		}
		if serverAddress == "" {
			return "", fmt.Errorf("k3s worker requires serverAddress or cluster controlPlaneEndpoint")
		}
	}

	// Set defaults for user configuration
	userName := kairosConfig.Spec.UserName
	if userName == "" {
		userName = "kairos"
	}
	userPassword := kairosConfig.Spec.UserPassword
	if userPassword == "" {
		userPassword = "kairos"
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
			Auto:   true,
			Device: "auto",
			Reboot: true,
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
	providerID := r.getProviderID(ctx, log, machine)

	// Build template data
	templateData := bootstrap.TemplateData{
		Role:                                role,
		SingleNode:                          singleNode,
		Hostname:                            hostname,
		UserName:                            userName,
		UserPassword:                        userPassword,
		UserGroups:                          userGroups,
		GitHubUser:                          kairosConfig.Spec.GitHubUser,
		SSHPublicKey:                        kairosConfig.Spec.SSHPublicKey,
		Manifests:                           kairosConfig.Spec.Manifests,
		HostnamePrefix:                      hostnamePrefix,
		DNSServers:                          kairosConfig.Spec.DNSServers,
		PrimaryIP:                           kairosConfig.Spec.PrimaryIP,
		MachineName:                         "",
		ClusterNS:                           "",
		IsKubeVirt:                          isKubevirtMachine(machine),
		Install:                             installConfig,
		ProviderID:                          providerID,
		K3sServerURL:                        serverAddress,
		K3sToken:                            k3sToken,
		ControlPlaneLBServiceName:           "",
		ControlPlaneLBServiceNamespace:      "",
		ControlPlaneLBEndpoint:              "",
		ManagementKubeconfigToken:           "",
		ManagementKubeconfigSecretName:      "",
		ManagementKubeconfigSecretNamespace: "",
		ManagementAPIServer:                 "",
	}

	if machine != nil {
		templateData.MachineName = machine.Name
	}
	if cluster != nil {
		templateData.ClusterNS = cluster.Namespace
	}

	return bootstrap.RenderK3sCloudConfig(templateData)
}

func (r *KairosConfigReconciler) getControlPlaneLBEndpoint(ctx context.Context, namespace, name string) (string, error) {
	if namespace == "" || name == "" {
		return "", nil
	}
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if len(service.Status.LoadBalancer.Ingress) == 0 {
		return "", nil
	}
	ingress := service.Status.LoadBalancer.Ingress[0]
	if ingress.IP != "" {
		return ingress.IP, nil
	}
	if ingress.Hostname != "" {
		return ingress.Hostname, nil
	}
	return "", nil
}

func (r *KairosConfigReconciler) reconcileDelete(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig) (ctrl.Result, error) {
	// Remove finalizer
	controllerutil.RemoveFinalizer(kairosConfig, bootstrapv1beta2.KairosConfigFinalizer)
	return ctrl.Result{}, r.Update(ctx, kairosConfig)
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// randomString generates a random lowercase alphanumeric string of the given length
// This ensures the string is RFC 1123 compliant for Kubernetes resource names
func randomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		randomByte := make([]byte, 1)
		if _, err := rand.Read(randomByte); err != nil {
			return "", err
		}
		b[i] = charset[randomByte[0]%byte(len(charset))]
	}
	return string(b), nil
}

type kubeconfigPushConfig struct {
	Token           string
	APIServer       string
	SecretName      string
	SecretNamespace string
}

func (r *KairosConfigReconciler) ensureKubeconfigPushConfig(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (*kubeconfigPushConfig, error) {
	if r.RESTConfig == nil || r.RESTConfig.Host == "" {
		log.Info("Skipping kubeconfig push config; REST config not available")
		return nil, nil
	}

	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	saName := kubeconfigWriterName(cluster.Name)

	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		if serviceAccount.Labels == nil {
			serviceAccount.Labels = map[string]string{}
		}
		serviceAccount.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kairosConfig, serviceAccount, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer serviceaccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{secretName},
				Verbs:         []string{"get", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"kubevirt.io"},
				Resources: []string{"virtualmachineinstances"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services"},
				Verbs:     []string{"get"},
			},
		}
		if role.Labels == nil {
			role.Labels = map[string]string{}
		}
		role.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kairosConfig, role, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer role: %w", err)
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     role.Name,
		}
		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccount.Name,
				Namespace: serviceAccount.Namespace,
			},
		}
		if roleBinding.Labels == nil {
			roleBinding.Labels = map[string]string{}
		}
		roleBinding.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kairosConfig, roleBinding, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer rolebinding: %w", err)
	}

	expirationSeconds := int64(24 * 60 * 60)
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"https://kubernetes.default.svc"},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	if err := r.SubResource("token").Create(ctx, serviceAccount, tokenRequest); err != nil {
		return nil, fmt.Errorf("failed to create serviceaccount token: %w", err)
	}
	if tokenRequest.Status.Token == "" {
		return nil, fmt.Errorf("serviceaccount token request returned empty token")
	}

	return &kubeconfigPushConfig{
		Token:           tokenRequest.Status.Token,
		APIServer:       r.RESTConfig.Host,
		SecretName:      secretName,
		SecretNamespace: cluster.Namespace,
	}, nil
}

func kubeconfigWriterName(clusterName string) string {
	base := "kairos-kubeconfig-writer"
	name := fmt.Sprintf("%s-%s", base, clusterName)
	if len(name) <= 63 {
		return name
	}
	hash := sha1.Sum([]byte(clusterName))
	suffix := hex.EncodeToString(hash[:6])
	maxClusterLen := 63 - len(base) - len(suffix) - 2
	if maxClusterLen < 1 {
		maxClusterLen = 1
	}
	trimmed := clusterName
	if len(trimmed) > maxClusterLen {
		trimmed = trimmed[:maxClusterLen]
	}
	return fmt.Sprintf("%s-%s-%s", base, trimmed, suffix)
}

// SetupWithManager sets up the controller with the Manager.
func (r *KairosConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	log := ctrl.Log.WithName("KairosConfig")

	// Create unstructured VSphereMachine object for watching
	vsphereMachineGVK := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "VSphereMachine",
	}
	vsphereMachine := &unstructured.Unstructured{}
	vsphereMachine.SetGroupVersionKind(vsphereMachineGVK)

	// Create unstructured KubevirtMachine objects for watching (v1alpha1 and v1alpha4)
	kubevirtMachineGVKAlpha1 := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "KubevirtMachine",
	}
	kubevirtMachineAlpha1 := &unstructured.Unstructured{}
	kubevirtMachineAlpha1.SetGroupVersionKind(kubevirtMachineGVKAlpha1)

	kubevirtMachineGVKAlpha4 := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha4",
		Kind:    "KubevirtMachine",
	}
	kubevirtMachineAlpha4 := &unstructured.Unstructured{}
	kubevirtMachineAlpha4.SetGroupVersionKind(kubevirtMachineGVKAlpha4)

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1beta2.KairosConfig{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToKairosConfig),
		).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.machineToKairosConfig),
		).
		Watches(
			vsphereMachine,
			handler.EnqueueRequestsFromMapFunc(r.vsphereMachineToKairosConfig),
		).
		Watches(
			kubevirtMachineAlpha1,
			handler.EnqueueRequestsFromMapFunc(r.kubevirtMachineToKairosConfig),
		)

	if r.gvkExists(mgr, kubevirtMachineGVKAlpha4) {
		builder = builder.Watches(
			kubevirtMachineAlpha4,
			handler.EnqueueRequestsFromMapFunc(r.kubevirtMachineToKairosConfig),
		)
	} else {
		log.V(2).Info("Skipping watch: KubevirtMachine v1alpha4 CRD not installed")
	}

	return builder.Complete(r)
}

func (r *KairosConfigReconciler) gvkExists(mgr ctrl.Manager, gvk schema.GroupVersionKind) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// secretToKairosConfig maps CAPK userdata secrets to their owning KairosConfig
func (r *KairosConfigReconciler) secretToKairosConfig(ctx context.Context, o client.Object) []reconcile.Request {
	secret, ok := o.(*corev1.Secret)
	if !ok {
		return nil
	}
	if !strings.HasSuffix(secret.Name, "-userdata") {
		return nil
	}
	if _, ok := secret.Labels[clusterv1.ClusterNameLabel]; !ok {
		return nil
	}

	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(secret.Namespace)); err != nil {
		return nil
	}

	bootstrapSecretName := strings.TrimSuffix(secret.Name, "-userdata")
	for _, machine := range machineList.Items {
		if machine.Spec.Bootstrap.DataSecretName == nil {
			continue
		}
		if *machine.Spec.Bootstrap.DataSecretName != bootstrapSecretName {
			continue
		}
		if machine.Spec.Bootstrap.ConfigRef == nil {
			continue
		}
		if machine.Spec.Bootstrap.ConfigRef.GroupVersionKind().Group != bootstrapv1beta2.GroupVersion.Group {
			continue
		}
		if machine.Spec.Bootstrap.ConfigRef.Kind != "KairosConfig" {
			continue
		}
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      machine.Spec.Bootstrap.ConfigRef.Name,
					Namespace: machine.Spec.Bootstrap.ConfigRef.Namespace,
				},
			},
		}
	}

	return nil
}

// machineToKairosConfig maps a Machine to its KairosConfig
func (r *KairosConfigReconciler) machineToKairosConfig(ctx context.Context, o client.Object) []reconcile.Request {
	machine, ok := o.(*clusterv1.Machine)
	if !ok {
		return nil
	}

	// Check if Machine has a bootstrap config reference
	if machine.Spec.Bootstrap.ConfigRef == nil {
		return nil
	}

	// Check if it's a KairosConfig
	if machine.Spec.Bootstrap.ConfigRef.GroupVersionKind().Group != bootstrapv1beta2.GroupVersion.Group {
		return nil
	}
	if machine.Spec.Bootstrap.ConfigRef.Kind != "KairosConfig" {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      machine.Spec.Bootstrap.ConfigRef.Name,
				Namespace: machine.Namespace,
			},
		},
	}
}

// vsphereMachineToKairosConfig maps a VSphereMachine to its KairosConfig
// This allows us to watch for VSphereMachine changes (especially when providerID is set)
// and trigger KairosConfig reconciliation to regenerate bootstrap secret with providerID
func (r *KairosConfigReconciler) vsphereMachineToKairosConfig(ctx context.Context, o client.Object) []reconcile.Request {
	// Verify this is an unstructured object (VSphereMachine)
	if _, ok := o.(*unstructured.Unstructured); !ok {
		return nil
	}

	// Get the Machine that owns this VSphereMachine
	// VSphereMachine is typically owned by a Machine
	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, machine := range machineList.Items {
		// Check if this Machine references the VSphereMachine
		if machine.Spec.InfrastructureRef.Kind == "VSphereMachine" &&
			machine.Spec.InfrastructureRef.Name == o.GetName() &&
			machine.Spec.InfrastructureRef.Namespace == o.GetNamespace() {
			// Check if Machine has a bootstrap config reference to KairosConfig
			if machine.Spec.Bootstrap.ConfigRef != nil &&
				machine.Spec.Bootstrap.ConfigRef.GroupVersionKind().Group == bootstrapv1beta2.GroupVersion.Group &&
				machine.Spec.Bootstrap.ConfigRef.Kind == "KairosConfig" {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      machine.Spec.Bootstrap.ConfigRef.Name,
						Namespace: machine.Spec.Bootstrap.ConfigRef.Namespace,
					},
				})
			}
		}
	}

	return requests
}

// kubevirtMachineToKairosConfig maps a KubevirtMachine to its KairosConfig
// This allows us to watch for KubevirtMachine changes (especially when providerID is set)
// and trigger KairosConfig reconciliation to regenerate bootstrap secret with providerID
func (r *KairosConfigReconciler) kubevirtMachineToKairosConfig(ctx context.Context, o client.Object) []reconcile.Request {
	// Verify this is an unstructured object (KubevirtMachine)
	if _, ok := o.(*unstructured.Unstructured); !ok {
		return nil
	}

	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, machine := range machineList.Items {
		if (machine.Spec.InfrastructureRef.Kind == "KubevirtMachine" || machine.Spec.InfrastructureRef.Kind == "KubeVirtMachine") &&
			machine.Spec.InfrastructureRef.Name == o.GetName() &&
			machine.Spec.InfrastructureRef.Namespace == o.GetNamespace() {
			if machine.Spec.Bootstrap.ConfigRef != nil &&
				machine.Spec.Bootstrap.ConfigRef.GroupVersionKind().Group == bootstrapv1beta2.GroupVersion.Group &&
				machine.Spec.Bootstrap.ConfigRef.Kind == "KairosConfig" {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      machine.Spec.Bootstrap.ConfigRef.Name,
						Namespace: machine.Spec.Bootstrap.ConfigRef.Namespace,
					},
				})
			}
		}
	}

	return requests
}

// getProviderID retrieves the providerID from the Machine's infrastructure reference
// This is used to configure k0s/kubelet to set the providerID on the Node
// so the Machine controller can match Nodes to Machines
func (r *KairosConfigReconciler) getProviderID(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) string {
	if machine == nil {
		log.V(4).Info("Machine is nil, cannot get providerID")
		return ""
	}

	// First, check if Machine already has providerID set
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		log.Info("Using providerID from Machine spec", "providerID", *machine.Spec.ProviderID, "machine", machine.Name)
		return *machine.Spec.ProviderID
	}

	// Try to get providerID from infrastructure reference (e.g., VSphereMachine)
	if machine.Spec.InfrastructureRef.Kind == "" {
		log.V(4).Info("Machine has no infrastructure reference, cannot get providerID", "machine", machine.Name)
		return ""
	}

	// For VSphere, get providerID from VSphereMachine spec
	if machine.Spec.InfrastructureRef.Kind == "VSphereMachine" {
		vsphereMachine := &unstructured.Unstructured{}
		vsphereMachine.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "infrastructure.cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "VSphereMachine",
		})
		vsphereMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereMachineKey, vsphereMachine); err != nil {
			log.V(4).Info("Failed to get VSphereMachine for providerID", "machine", machine.Name, "vsphereMachine", vsphereMachineKey.Name, "error", err)
			return ""
		}

		// Try to get providerID from spec.providerID first (most reliable)
		if providerID, found, err := unstructured.NestedString(vsphereMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			log.V(4).Info("Found providerID in VSphereMachine spec", "providerID", providerID, "machine", machine.Name, "vsphereMachine", vsphereMachineKey.Name)
			return providerID
		}

		// Try to get VM UUID from status and construct providerID
		// This is set by CAPV after VM is provisioned
		if vmUUID, found, err := unstructured.NestedString(vsphereMachine.Object, "status", "vmUUID"); err == nil && found && vmUUID != "" {
			providerID := fmt.Sprintf("vsphere://%s", vmUUID)
			log.V(4).Info("Constructed providerID from VSphereMachine VM UUID", "providerID", providerID, "vmUUID", vmUUID, "machine", machine.Name, "vsphereMachine", vsphereMachineKey.Name)
			return providerID
		}

		// Check status.providerID as well (some CAPV versions set this)
		if providerID, found, err := unstructured.NestedString(vsphereMachine.Object, "status", "providerID"); err == nil && found && providerID != "" {
			log.V(4).Info("Found providerID in VSphereMachine status", "providerID", providerID, "machine", machine.Name, "vsphereMachine", vsphereMachineKey.Name)
			return providerID
		}

		log.Info("VSphereMachine found but no providerID or vmUUID available yet", "machine", machine.Name, "vsphereMachine", vsphereMachineKey.Name)
	}

	// For CAPK, get providerID from KubevirtMachine spec
	if machine.Spec.InfrastructureRef.Kind == "KubevirtMachine" || machine.Spec.InfrastructureRef.Kind == "KubeVirtMachine" {
		kubevirtMachine := &unstructured.Unstructured{}
		kubevirtMachineGVK := machine.Spec.InfrastructureRef.GroupVersionKind()
		if kubevirtMachineGVK.Group == "" || kubevirtMachineGVK.Version == "" {
			kubevirtMachineGVK = schema.GroupVersionKind{
				Group:   "infrastructure.cluster.x-k8s.io",
				Version: "v1alpha1",
				Kind:    "KubevirtMachine",
			}
		}
		kubevirtMachine.SetGroupVersionKind(kubevirtMachineGVK)
		kubevirtMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, kubevirtMachineKey, kubevirtMachine); err != nil {
			log.V(4).Info("Failed to get KubevirtMachine for providerID", "machine", machine.Name, "kubevirtMachine", kubevirtMachineKey.Name, "error", err)
			return ""
		}

		if providerID, found, err := unstructured.NestedString(kubevirtMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			log.V(4).Info("Found providerID in KubevirtMachine spec", "providerID", providerID, "machine", machine.Name, "kubevirtMachine", kubevirtMachineKey.Name)
			return providerID
		}
	}

	// For CAPD, get providerID from DockerMachine spec
	if machine.Spec.InfrastructureRef.Kind == "DockerMachine" {
		dockerMachine := &unstructured.Unstructured{}
		dockerMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		dockerMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, dockerMachineKey, dockerMachine); err != nil {
			log.V(4).Info("Failed to get DockerMachine for providerID", "machine", machine.Name, "dockerMachine", dockerMachineKey.Name, "error", err)
			return ""
		}
		if providerID, found, err := unstructured.NestedString(dockerMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			log.V(4).Info("Found providerID in DockerMachine spec", "providerID", providerID, "machine", machine.Name, "dockerMachine", dockerMachineKey.Name)
			return providerID
		}
	}

	return ""
}
