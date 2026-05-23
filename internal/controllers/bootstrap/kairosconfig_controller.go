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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/bootstrap"
)

const controlPlaneLBServiceSuffix = "control-plane-lb"

var errLBEndpointNotReady = errors.New("control plane load balancer endpoint not ready")
var errK3sTokenNotReady = errors.New("k3s token secret not ready")

// KairosConfigReconciler reconciles a KairosConfig object.
//
// MgmtEndpointResolver is the seam introduced by KD-33: the reconciler holds
// an adapter that turns a (KairosConfig, Cluster) tuple into the four-field
// management-endpoint bundle the renderer needs to emit the in-node
// kubeconfig-push block on CAPK control-plane Machines. In production
// (main.go) this is a kubeVirtTokenResolver wired off mgr.GetConfig().Host.
// In tests it can be a fake. The reconciler MUST tolerate the resolver
// returning (nil, nil) — that is the documented "disabled" signal, used by
// envtest setups and the out-of-cluster `go run` flow where the manager has
// no usable REST config to point nodes back at.
type KairosConfigReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	MgmtEndpointResolver ManagementEndpointResolver
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
//
// The function uses a single deferred patch.Helper.Patch to flush all spec and
// status mutations on the way out. The helper is created immediately after the
// initial Get so that even early-return paths (paused, no owner Machine, no
// Cluster) reconcile observedGeneration and conditions. This closes KD-14.
//
// Named returns (result, retErr) are required so the deferred closure can
// combine the patch error into the returned error via errors.Join.
func (r *KairosConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the KairosConfig instance
	kairosConfig := &bootstrapv1beta2.KairosConfig{}
	if err := r.Get(ctx, req.NamespacedName, kairosConfig); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize patch helper BEFORE any early returns so paused/no-owner/no-Cluster
	// paths still flush observedGeneration and condition transitions. (KD-14.)
	patchHelper, err := patch.NewHelper(kairosConfig, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always update observedGeneration, including on early-return paths.
	kairosConfig.Status.ObservedGeneration = kairosConfig.Generation

	// patchOnExit lets reconcileDelete signal that it has already issued a bare
	// r.Update for the terminal finalizer-removal step and the deferred patch
	// MUST be skipped to avoid racing with the apiserver removing the object.
	// In commit 1 this is always true; commit 4 wires the skip path.
	patchOnExit := true
	defer func() {
		if !patchOnExit {
			return
		}
		if perr := patchHelper.Patch(ctx, kairosConfig); perr != nil {
			retErr = errors.Join(retErr, perr)
		}
	}()

	// Handle deletion. reconcileDelete signals via skipPatch=true when it
	// has just issued a bare r.Update for the terminal finalizer-remove
	// step -- the deferred Patch must be skipped to avoid racing the
	// apiserver removing the object.
	if !kairosConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		res, skipPatch, derr := r.reconcileDelete(ctx, log, kairosConfig)
		if skipPatch {
			patchOnExit = false
		}
		return res, derr
	}

	// Add finalizer if needed. The deferred patch helper flushes the change;
	// no bare r.Update here. (controller-reconcile-safety skill.)
	controllerutil.AddFinalizer(kairosConfig, bootstrapv1beta2.KairosConfigFinalizer)

	// Check if paused. observedGeneration was already set above; the deferred
	// patch flushes it along with any condition changes a previous Reconcile
	// left in flight. FailureReason/FailureMessage are intentionally NOT
	// cleared on the paused path -- latched failure state still reflects the
	// last real observation and clears on the first successful post-resume
	// Reconcile.
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

	// Reconcile bootstrap data
	bootstrapResult, err := r.reconcileBootstrapData(ctx, log, kairosConfig, machine, cluster)
	if err != nil {
		// Mark conditions as false on error
		// Use "%s" as format string and pass error as argument to satisfy linter
		conditions.MarkFalse(kairosConfig, clusterv1.ReadyCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		conditions.MarkFalse(kairosConfig, bootstrapv1beta2.BootstrapReadyCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		conditions.MarkFalse(kairosConfig, bootstrapv1beta2.DataSecretAvailableCondition, bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())

		kairosConfig.Status.FailureReason = bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason
		kairosConfig.Status.FailureMessage = err.Error()
		kairosConfig.Status.Ready = false

		return ctrl.Result{}, nil
	}

	// If reconcileBootstrapData requested a requeue (e.g., waiting for providerID), return it
	if bootstrapResult.Requeue || bootstrapResult.RequeueAfter > 0 {
		return bootstrapResult, nil
	}

	// Mark conditions as true on success
	conditions.MarkTrue(kairosConfig, clusterv1.ReadyCondition)
	conditions.MarkTrue(kairosConfig, bootstrapv1beta2.BootstrapReadyCondition)
	conditions.MarkTrue(kairosConfig, bootstrapv1beta2.DataSecretAvailableCondition)

	// Clear failure fields on every successful exit so a transient missing
	// dependency (e.g. userPasswordSecretRef target Secret) does not latch
	// FailureReason/Message into a terminal state that blocks the CAPI Machine
	// controller from cloning the infrastructure Machine. (KD-14.)
	kairosConfig.Status.FailureReason = ""
	kairosConfig.Status.FailureMessage = ""

	return ctrl.Result{}, nil
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
					// Ensure SSH enable stage exists (regression guard for CAPV access).
					//
					// KD-3a: the `hasSSHPassAuth` check that previously looked for
					// `PasswordAuthentication yes` is gone. That injection was
					// deliberately removed in KD-3a; keeping the substring check
					// caused an infinite regeneration loop (the check always fired
					// "missing", controller rewrote the secret every reconcile).
					// The whole substring-based regeneration heuristic is KD-9 —
					// replacing it with a template-version annotation on the
					// Secret is the proper long-term fix.
					hasSSHEnableStage := strings.Contains(cloudConfigStr, "systemctl enable --now sshd") ||
						strings.Contains(cloudConfigStr, "systemctl enable --now ssh")

					if currentProviderID != "" && (!hasProviderIDInSecret || !hasPostBootstrapService) {
						log.Info("Bootstrap secret missing providerID in post-bootstrap service, regenerating to include it",
							"secret", *kairosConfig.Status.DataSecretName,
							"providerID", currentProviderID,
							"hasProviderIDInSecret", hasProviderIDInSecret,
							"hasPostBootstrapService", hasPostBootstrapService)
						needsRegeneration = true
					}
					if !hasSSHEnableStage {
						log.Info("Bootstrap secret missing SSH enable stage, regenerating",
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

// resolveUserPassword returns the user password for the default user, in
// precedence order: UserPasswordSecretRef > inline UserPassword > "" (empty).
//
// Empty is a valid return value here. The validating webhook requires the
// KairosConfig to set at least one of userPassword/userPasswordSecretRef/
// sshPublicKey/gitHubUser, so an empty password just means the user opted
// for SSH-key-only auth. The cloud-config templates handle that by simply
// not emitting a `passwd:` field for the default user.
//
// KD-3a, v0.1.0-alpha.2: this replaced the previous behaviour of defaulting
// to "kairos" when no password was set.
func (r *KairosConfigReconciler) resolveUserPassword(ctx context.Context, kairosConfig *bootstrapv1beta2.KairosConfig) (string, error) {
	if ref := kairosConfig.Spec.UserPasswordSecretRef; ref != nil && ref.Name != "" {
		secretKey := types.NamespacedName{
			Namespace: kairosConfig.Namespace,
			Name:      ref.Name,
		}
		if ref.Namespace != "" {
			secretKey.Namespace = ref.Namespace
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return "", fmt.Errorf("get user password secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
		}
		key := ref.Key
		if key == "" {
			key = "password"
		}
		data, ok := secret.Data[key]
		if !ok {
			return "", fmt.Errorf("user password secret %s/%s does not contain key %q", secretKey.Namespace, secretKey.Name, key)
		}
		return string(data), nil
	}
	return kairosConfig.Spec.UserPassword, nil
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

	// CAPK control-plane: ask the resolver for the management-endpoint bundle
	// the node needs to push its kubeconfig back without SSH. The gating
	// (CAPK + control-plane only) stays here in the reconciler — it's
	// routing, not a resolver concern. The resolver itself is allowed to
	// return (nil, nil) as a "disabled" signal (e.g. envtest without a
	// management REST config); we treat that the same as "no push block",
	// per the contract in management_endpoint.go.
	var mgmtEndpoint *ManagementEndpoint
	if isKubevirtMachine(machine) && role == "control-plane" && r.MgmtEndpointResolver != nil {
		var err error
		mgmtEndpoint, err = r.MgmtEndpointResolver.Resolve(ctx, kairosConfig, cluster)
		if err != nil {
			return "", err
		}
	}

	// Build template data
	templateData := bootstrap.TemplateData{
		Role:                           role,
		SingleNode:                     singleNode,
		Hostname:                       hostname,
		UserName:                       userName,
		UserPassword:                   userPassword,
		UserGroups:                     userGroups,
		GitHubUser:                     kairosConfig.Spec.GitHubUser,
		SSHPublicKey:                   kairosConfig.Spec.SSHPublicKey,
		WorkerToken:                    workerToken,
		Manifests:                      kairosConfig.Spec.Manifests,
		HostnamePrefix:                 hostnamePrefix,
		DNSServers:                     kairosConfig.Spec.DNSServers,
		PodCIDR:                        kairosConfig.Spec.PodCIDR,
		ServiceCIDR:                    kairosConfig.Spec.ServiceCIDR,
		PrimaryIP:                      kairosConfig.Spec.PrimaryIP,
		MachineName:                    "",
		ClusterNS:                      "",
		IsKubeVirt:                     isKubevirtMachine(machine),
		Install:                        installConfig,
		ProviderID:                     providerID,
		ControlPlaneLBServiceName:      "",
		ControlPlaneLBServiceNamespace: "",
		ControlPlaneLBEndpoint:         "",
	}
	if mgmtEndpoint != nil {
		// One-line conversion preserves the rule that internal/bootstrap is
		// API-server-unaware: the renderer's ManagementEndpoint is a flat
		// data struct, identical in shape but distinct in type.
		//
		// ClusterName and ControlPlaneEndpointHost are stamped from the live
		// Cluster object rather than the resolver output because they're pure
		// CAPI metadata (not resolver-specific): the cluster-name label keeps
		// the controlplane controller's Secret-watch predicate sharp, and the
		// CP endpoint host is what CAPV's `server:` URL rewrite uses. Keeping
		// these in the call site means the resolver doesn't need to be aware
		// of the rewrite semantics.
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

	// CAPK control-plane: same routing as the k0s path above.
	var mgmtEndpoint *ManagementEndpoint
	if isKubevirtMachine(machine) && role == "control-plane" && r.MgmtEndpointResolver != nil {
		var err error
		mgmtEndpoint, err = r.MgmtEndpointResolver.Resolve(ctx, kairosConfig, cluster)
		if err != nil {
			return "", err
		}
	}

	// Build template data
	templateData := bootstrap.TemplateData{
		Role:                           role,
		SingleNode:                     singleNode,
		Hostname:                       hostname,
		UserName:                       userName,
		UserPassword:                   userPassword,
		UserGroups:                     userGroups,
		GitHubUser:                     kairosConfig.Spec.GitHubUser,
		SSHPublicKey:                   kairosConfig.Spec.SSHPublicKey,
		Manifests:                      kairosConfig.Spec.Manifests,
		HostnamePrefix:                 hostnamePrefix,
		DNSServers:                     kairosConfig.Spec.DNSServers,
		PrimaryIP:                      kairosConfig.Spec.PrimaryIP,
		MachineName:                    "",
		ClusterNS:                      "",
		IsKubeVirt:                     isKubevirtMachine(machine),
		Install:                        installConfig,
		ProviderID:                     providerID,
		K3sServerURL:                   serverAddress,
		K3sToken:                       k3sToken,
		ControlPlaneLBServiceName:      "",
		ControlPlaneLBServiceNamespace: "",
		ControlPlaneLBEndpoint:         "",
	}
	if mgmtEndpoint != nil {
		// See k0s twin above for the rationale behind stamping ClusterName /
		// ControlPlaneEndpointHost from the live Cluster (not the resolver).
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

// reconcileDelete drains the owned bootstrap Secret before removing the
// finalizer. Stripping the finalizer with the Secret still attached leaks the
// rendered cloud-config (containing user passwords and tokens) until
// Kubernetes garbage collection eventually reaps it -- the failure mode
// motivating KD-4.
//
// Returns (result, skipPatch, err). When skipPatch is true the caller MUST
// NOT run the deferred patch.Helper.Patch -- this happens when we just
// issued a bare r.Update for the terminal finalizer-remove step. Once the
// last finalizer drops, the apiserver is free to garbage-collect the
// object, and a follow-up patch from the deferred closure would race that
// GC and surface as a 404.
func (r *KairosConfigReconciler) reconcileDelete(ctx context.Context, log logr.Logger, kairosConfig *bootstrapv1beta2.KairosConfig) (result ctrl.Result, skipPatch bool, err error) {
	gone, err := deleteBootstrapSecret(ctx, r.Client, kairosConfig)
	if err != nil {
		log.Error(err, "Failed to delete bootstrap Secret",
			"kairosConfig", kairosConfig.Name,
			"namespace", kairosConfig.Namespace,
			"uid", kairosConfig.UID)
		return ctrl.Result{}, false, err
	}
	if !gone {
		log.Info("Waiting for bootstrap Secret to be reaped before removing KairosConfig finalizer",
			"kairosConfig", kairosConfig.Name,
			"namespace", kairosConfig.Namespace,
			"secret", *kairosConfig.Status.DataSecretName)
		return ctrl.Result{RequeueAfter: bootstrapDeleteRequeueAfter}, false, nil
	}

	// Terminal step: remove the finalizer with a bare r.Update. The patch
	// helper is bypassed via skipPatch=true so the deferred closure in
	// Reconcile does not race the apiserver's garbage-collection of this
	// object. (KD-4, per maintainer-confirmed plan.)
	controllerutil.RemoveFinalizer(kairosConfig, bootstrapv1beta2.KairosConfigFinalizer)
	if err := r.Update(ctx, kairosConfig); err != nil {
		if apierrors.IsNotFound(err) {
			// Object already gone.
			return ctrl.Result{}, true, nil
		}
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}

// bootstrapDeleteRequeueAfter is the polling interval while waiting for the
// owned bootstrap Secret to be garbage-collected. Short because Secret GC
// is normally near-instant; long enough not to hammer the apiserver.
const bootstrapDeleteRequeueAfter = 5 * time.Second

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

// kubeconfigPushConfig + ensureKubeconfigPushConfig were moved out to
// management_endpoint_resolver.go's kubeVirtTokenResolver implementation as
// part of KD-33. The reconciler now holds an interface-typed
// MgmtEndpointResolver field and routes through it; main.go wires the
// production kubeVirtTokenResolver, tests wire fakes.

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
// optionalInfraWatches lists infrastructure machine CRDs that the controller watches
// when present. Each entry is tried at startup; missing CRDs are silently skipped.
// When an infrastructure machine changes (e.g. providerID is set), the controller
// re-reconciles the owning KairosConfig to regenerate the bootstrap secret.
var optionalInfraWatches = []schema.GroupVersionKind{
	{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "VSphereMachine"},
	{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "KubevirtMachine"},
}

func (r *KairosConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	log := ctrl.Log.WithName("KairosConfig")

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1beta2.KairosConfig{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToKairosConfig),
		).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.machineToKairosConfig),
		)

	for _, gvk := range optionalInfraWatches {
		if !r.gvkExists(mgr, gvk) {
			log.V(2).Info("Skipping watch: CRD not installed", "kind", gvk.Kind, "version", gvk.Version)
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		builder = builder.Watches(obj, handler.EnqueueRequestsFromMapFunc(r.infraMachineToKairosConfig))
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

// infraMachineToKairosConfig maps any infrastructure machine (VSphereMachine, KubevirtMachine, etc.)
// to its owning KairosConfig. This triggers re-reconciliation when the infra machine changes
// (e.g. providerID is set), so the bootstrap secret can be regenerated.
func (r *KairosConfigReconciler) infraMachineToKairosConfig(ctx context.Context, o client.Object) []reconcile.Request {
	if _, ok := o.(*unstructured.Unstructured); !ok {
		return nil
	}
	infraKind := o.GetObjectKind().GroupVersionKind().Kind

	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, machine := range machineList.Items {
		ref := machine.Spec.InfrastructureRef
		if ref.Kind == infraKind &&
			ref.Name == o.GetName() && ref.Namespace == o.GetNamespace() &&
			machine.Spec.Bootstrap.ConfigRef != nil &&
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
