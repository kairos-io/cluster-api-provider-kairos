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
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1beta2 "github.com/kairos-io/kairos-capi/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/kairos-capi/api/controlplane/v1beta2"
	"github.com/kairos-io/kairos-capi/internal/infrastructure"
)

// KairosControlPlaneReconciler reconciles a KairosControlPlane object
type KairosControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const controlPlaneLBServiceSuffix = "control-plane-lb"

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status;machines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs;kairosconfigtemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;endpoints,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *KairosControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the KairosControlPlane instance
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, kcp)
	}

	// Add finalizer if needed
	if !controllerutil.ContainsFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer) {
		controllerutil.AddFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer)
		if err := r.Update(ctx, kcp); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Find the owning Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		// If the error is due to missing cluster-name label or Cluster not found,
		// try to find the Cluster by searching for Clusters that reference this KairosControlPlane
		errMsg := err.Error()
		if errMsg == "no \"cluster.x-k8s.io/cluster-name\" label present" ||
			apierrors.IsNotFound(err) ||
			(errMsg != "" && (errMsg == "failed to get Cluster/kairos-cluster: Cluster.cluster.x-k8s.io \"kairos-cluster\" not found" ||
				errMsg == "Cluster.cluster.x-k8s.io \"kairos-cluster\" not found")) {
			log.Info("cluster-name label missing or Cluster not found via metadata, searching for Cluster that references this control plane", "error", errMsg)
			cluster, err = r.findClusterForControlPlane(ctx, log, kcp)
			if err != nil {
				log.Error(err, "Failed to find cluster for control plane")
				return ctrl.Result{}, err
			}
			if cluster != nil {
				// Set the label on the KairosControlPlane
				if kcp.Labels == nil {
					kcp.Labels = make(map[string]string)
				}
				kcp.Labels[clusterv1.ClusterNameLabel] = cluster.Name
				if err := r.Update(ctx, kcp); err != nil {
					log.Error(err, "Failed to update KairosControlPlane with cluster-name label")
					return ctrl.Result{}, err
				}
				log.Info("Set cluster-name label on KairosControlPlane", "cluster", cluster.Name)
				// Return to trigger a new reconcile with the label set
				return ctrl.Result{Requeue: true}, nil
			}
		} else {
			log.Error(err, "Failed to get cluster from metadata")
			return ctrl.Result{}, err
		}
	}
	if cluster == nil {
		log.Info("Cluster is not available yet")
		return ctrl.Result{}, nil
	}

	// Always update observedGeneration
	kcp.Status.ObservedGeneration = kcp.Generation

	// Reconcile control plane machines
	if err := r.reconcileMachines(ctx, log, kcp, cluster); err != nil {
		// Use "%s" as format string and pass error as argument to satisfy linter
		conditions.MarkFalse(kcp, clusterv1.ReadyCondition, controlplanev1beta2.ControlPlaneInitializationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		conditions.MarkFalse(kcp, controlplanev1beta2.AvailableCondition, controlplanev1beta2.ControlPlaneInitializationFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		kcp.Status.FailureReason = controlplanev1beta2.ControlPlaneInitializationFailedReason
		kcp.Status.FailureMessage = err.Error()
		// Use Status().Update() to ensure all status fields are included
		if updateErr := r.Status().Update(ctx, kcp); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update KCP status: %w", updateErr)
		}
		return ctrl.Result{}, nil
	}

	// Track previous initialized state to detect transitions
	wasInitialized := kcp.Status.Initialized

	// Update status
	if err := r.updateStatus(ctx, log, kcp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the control-plane LoadBalancer Service exists for KubeVirt clusters.
	if isKubevirtControlPlane(kcp) {
		if err := r.reconcileControlPlaneLB(ctx, log, kcp, cluster); err != nil {
			log.Error(err, "Failed to reconcile control plane load balancer service")
		}
	}

	// Retrieve and store kubeconfig if control plane infrastructure is ready
	// We check infrastructure readiness as a fallback even if NodeRef isn't set yet
	// This allows us to retrieve kubeconfig before the node is fully registered
	shouldRetrieveKubeconfig := false
	if kcp.Status.ReadyReplicas > 0 && kcp.Status.Initialized {
		shouldRetrieveKubeconfig = true
		log.Info("Control plane is ready (NodeRef set), attempting to retrieve kubeconfig",
			"readyReplicas", kcp.Status.ReadyReplicas,
			"initialized", kcp.Status.Initialized)
	} else {
		// Fallback: Check if infrastructure is ready even without NodeRef
		// This is useful when k0s is running but node hasn't registered yet
		// But first check if kubeconfig already exists to avoid unnecessary work
		secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
		secretKey := types.NamespacedName{
			Name:      secretName,
			Namespace: cluster.Namespace,
		}
		existingSecret := &corev1.Secret{}
		if err := r.Get(ctx, secretKey, existingSecret); err == nil {
			if kubeconfig, ok := existingSecret.Data["value"]; ok && len(kubeconfig) > 0 {
				// Kubeconfig already exists, skip retrieval
				log.V(4).Info("Kubeconfig already exists, skipping retrieval",
					"readyReplicas", kcp.Status.ReadyReplicas,
					"initialized", kcp.Status.Initialized)
			} else {
				// Kubeconfig secret exists but is empty, try to retrieve
				machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
				if err == nil && len(machines) > 0 {
					for _, machine := range machines {
						// Check if infrastructure is ready
						if conditions.IsTrue(machine, clusterv1.InfrastructureReadyCondition) &&
							conditions.IsTrue(machine, clusterv1.BootstrapReadyCondition) {
							// Infrastructure and bootstrap are ready, try to retrieve kubeconfig
							shouldRetrieveKubeconfig = true
							log.Info("Control plane infrastructure ready (fallback), attempting to retrieve kubeconfig",
								"machine", machine.Name,
								"infrastructureReady", conditions.IsTrue(machine, clusterv1.InfrastructureReadyCondition),
								"bootstrapReady", conditions.IsTrue(machine, clusterv1.BootstrapReadyCondition))
							break
						}
					}
				}
			}
		} else if apierrors.IsNotFound(err) {
			// Kubeconfig doesn't exist, try to retrieve if infrastructure is ready
			machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
			if err == nil && len(machines) > 0 {
				for _, machine := range machines {
					// Check if infrastructure is ready
					if conditions.IsTrue(machine, clusterv1.InfrastructureReadyCondition) &&
						conditions.IsTrue(machine, clusterv1.BootstrapReadyCondition) {
						// Infrastructure and bootstrap are ready, try to retrieve kubeconfig
						shouldRetrieveKubeconfig = true
						log.Info("Control plane infrastructure ready (fallback), attempting to retrieve kubeconfig",
							"machine", machine.Name,
							"infrastructureReady", conditions.IsTrue(machine, clusterv1.InfrastructureReadyCondition),
							"bootstrapReady", conditions.IsTrue(machine, clusterv1.BootstrapReadyCondition))
						break
					}
				}
			}
		}
		if !shouldRetrieveKubeconfig {
			log.V(4).Info("Control plane not ready yet, skipping kubeconfig retrieval",
				"readyReplicas", kcp.Status.ReadyReplicas,
				"initialized", kcp.Status.Initialized)
		}
	}

	if shouldRetrieveKubeconfig {
		if err := r.reconcileKubeconfig(ctx, log, kcp, cluster); err != nil {
			// Check if this is a transient error that should be retried
			errMsg := err.Error()
			isTransientError := false
			retryDelay := 30 * time.Second // Default retry delay

			// Check for transient errors that indicate k0s/k3s is not ready yet or VM is rebooting
			if strings.Contains(errMsg, "k0s is not ready") ||
				strings.Contains(errMsg, "k0s service is not active") ||
				strings.Contains(errMsg, "k3s is not ready") ||
				strings.Contains(errMsg, "k3s service is not active") ||
				(strings.Contains(errMsg, "admin config") && strings.Contains(errMsg, "not found")) ||
				strings.Contains(errMsg, "connection refused") ||
				(strings.Contains(errMsg, "dial") && strings.Contains(errMsg, "failed")) ||
				strings.Contains(errMsg, "no such host") ||
				strings.Contains(errMsg, "still be initializing") {
				isTransientError = true
				log.Info("Transient error during kubeconfig retrieval, will retry",
					"error", errMsg,
					"retryDelay", retryDelay)
			}

			if isTransientError {
				// Requeue with delay to retry later
				// This allows k0s/k3s to finish initializing after Kairos reboots
				log.Info("Requeuing kubeconfig retrieval due to transient error",
					"readyReplicas", kcp.Status.ReadyReplicas,
					"initialized", kcp.Status.Initialized,
					"retryDelay", retryDelay)
				// Update status before requeuing to ensure progress is tracked
				if err := r.updateStatus(ctx, log, kcp, cluster); err != nil {
					log.Error(err, "Failed to update status before requeue")
				}
				// Use Status().Update() to ensure all status fields are included
				if updateErr := r.Status().Update(ctx, kcp); updateErr != nil {
					if apierrors.IsConflict(updateErr) {
						log.V(4).Info("Conflict updating KCP status before requeue, will retry", "error", updateErr)
						return ctrl.Result{Requeue: true}, nil
					}
					log.Error(updateErr, "Failed to update KCP status before requeue")
					return ctrl.Result{RequeueAfter: retryDelay}, nil
				}
				return ctrl.Result{RequeueAfter: retryDelay}, nil
			} else {
				log.Error(err, "Failed to reconcile kubeconfig (non-transient error)",
					"readyReplicas", kcp.Status.ReadyReplicas,
					"initialized", kcp.Status.Initialized)
				// Don't fail the reconcile for non-transient errors, just log
			}
		}
	}

	// Ensure Node providerID is set in the workload cluster once kubeconfig is available.
	// This avoids relying on in-VM scripts and unblocks NodeRef reconciliation.
	if err := r.ensureProviderIDOnNodes(ctx, log, kcp, cluster); err != nil {
		log.Error(err, "Failed to ensure providerID on workload nodes")
	}

	// Update Cluster status
	if err := r.updateClusterStatus(ctx, log, kcp, cluster); err != nil {
		log.Error(err, "Failed to update cluster status")
		// Don't fail the reconcile, just log the error
	}

	// Update conditions based on status
	if kcp.Status.Initialized {
		conditions.MarkTrue(kcp, clusterv1.ReadyCondition)
		conditions.MarkTrue(kcp, controlplanev1beta2.AvailableCondition)
		if kcp.Status.ReadyReplicas > 0 {
			conditions.MarkTrue(kcp, clusterv1.ReadyCondition)
		} else {
			conditions.MarkFalse(kcp, clusterv1.ReadyCondition, controlplanev1beta2.WaitingForMachinesReadyReason, clusterv1.ConditionSeverityInfo, "Waiting for control plane machines to be ready")
		}
	} else {
		conditions.MarkFalse(kcp, clusterv1.ReadyCondition, controlplanev1beta2.WaitingForMachinesReason, clusterv1.ConditionSeverityInfo, "Waiting for control plane initialization")
		conditions.MarkFalse(kcp, controlplanev1beta2.AvailableCondition, controlplanev1beta2.WaitingForMachinesReason, clusterv1.ConditionSeverityInfo, "Waiting for control plane initialization")
	}

	// Clear failure fields if successful
	if kcp.Status.ReadyReplicas > 0 {
		kcp.Status.FailureReason = ""
		kcp.Status.FailureMessage = ""
	}

	// Use Status().Update() instead of Patch() to ensure all status fields are included
	// This is important because Patch() with omitempty tags may omit zero values,
	// causing fields like ReadyReplicas to appear as null instead of 0
	// Status().Update() sends the complete status object, ensuring all fields are present
	if err := r.Status().Update(ctx, kcp); err != nil {
		if apierrors.IsConflict(err) {
			// Conflict means the object was modified, requeue to retry
			log.V(4).Info("Conflict updating KCP status, will requeue", "error", err)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update KCP status: %w", err)
	}

	log.Info("Successfully updated KCP status",
		"initialized", kcp.Status.Initialized,
		"readyReplicas", kcp.Status.ReadyReplicas,
		"replicas", kcp.Status.Replicas,
		"updatedReplicas", kcp.Status.UpdatedReplicas,
		"unavailableReplicas", kcp.Status.UnavailableReplicas,
		"observedGeneration", kcp.Status.ObservedGeneration)

	// Trigger Cluster reconciliation when status.Initialized transitions from false to true
	// This ensures the Cluster controller promptly sets ControlPlaneInitialized condition
	// We do this AFTER persisting the KCP status to ensure the Cluster controller sees the updated status
	if !wasInitialized && kcp.Status.Initialized {
		log.Info("Control plane initialized state changed, triggering Cluster reconciliation", "cluster", cluster.Name)
		if err := r.triggerClusterReconciliation(ctx, log, cluster); err != nil {
			log.V(4).Info("Failed to trigger Cluster reconciliation", "error", err)
			// Don't fail the reconcile, just log - Cluster controller will eventually reconcile
		}
	}

	return ctrl.Result{}, nil
}

// findClusterForControlPlane searches for a Cluster that references this KairosControlPlane
func (r *KairosControlPlaneReconciler) findClusterForControlPlane(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) (*clusterv1.Cluster, error) {
	// List all Clusters in the same namespace
	clusters := &clusterv1.ClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(kcp.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}

	// Find the Cluster that references this KairosControlPlane
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.Spec.ControlPlaneRef != nil &&
			cluster.Spec.ControlPlaneRef.Kind == "KairosControlPlane" &&
			cluster.Spec.ControlPlaneRef.Name == kcp.Name {
			// Check namespace - it might be empty (defaults to cluster namespace)
			refNamespace := cluster.Spec.ControlPlaneRef.Namespace
			if refNamespace == "" || refNamespace == kcp.Namespace {
				// Check API version/group matches
				// In v1beta2, ControlPlaneRef uses apiGroup in YAML, but Go type uses APIVersion
				// When apiGroup is set, APIVersion may be empty or contain the full version string
				refAPIVersion := cluster.Spec.ControlPlaneRef.APIVersion
				expectedGroup := controlplanev1beta2.GroupVersion.Group
				expectedVersion := controlplanev1beta2.GroupVersion.String()

				// Match if:
				// 1. APIVersion is empty (v1beta2 using apiGroup - we trust the kind match)
				// 2. APIVersion matches expected version (v1beta1 style or v1beta2 with full version)
				// 3. APIVersion contains the expected group (handles partial matches)
				if refAPIVersion == "" {
					// Empty APIVersion means apiGroup is being used - trust the kind match
					log.Info("Found Cluster with matching ControlPlaneRef (apiGroup)", "cluster", cluster.Name, "kind", cluster.Spec.ControlPlaneRef.Kind)
					return cluster, nil
				}
				if refAPIVersion == expectedVersion {
					return cluster, nil
				}
				if len(refAPIVersion) > 0 && len(expectedGroup) > 0 && len(refAPIVersion) >= len(expectedGroup) && refAPIVersion[:len(expectedGroup)] == expectedGroup {
					return cluster, nil
				}
				log.Info("Cluster ControlPlaneRef APIVersion doesn't match", "cluster", cluster.Name, "refAPIVersion", refAPIVersion, "expectedVersion", expectedVersion, "expectedGroup", expectedGroup)
			}
		}
	}

	return nil, nil
}

func (r *KairosControlPlaneReconciler) reconcileMachines(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	// Get desired replica count
	desiredReplicas := int32(1)
	if kcp.Spec.Replicas != nil {
		desiredReplicas = *kcp.Spec.Replicas
	}

	// List existing control plane machines
	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return fmt.Errorf("failed to list control plane machines: %w", err)
	}

	// Sort machines by creation timestamp (oldest first) for stable operations
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].CreationTimestamp.Before(&machines[j].CreationTimestamp)
	})

	currentReplicas := int32(len(machines))

	log.Info("Reconciling control plane machines", "desired", desiredReplicas, "current", currentReplicas)

	maxSurge := int32(1)
	if kcp.Spec.RolloutStrategy != nil && kcp.Spec.RolloutStrategy.RollingUpdate != nil && kcp.Spec.RolloutStrategy.RollingUpdate.MaxSurge != nil {
		maxSurge = *kcp.Spec.RolloutStrategy.RollingUpdate.MaxSurge
	}

	outdatedMachines := make([]*clusterv1.Machine, 0)
	updatedReadyReplicas := int32(0)
	for _, machine := range machines {
		if r.machineMatchesVersion(machine, kcp.Spec.Version) {
			if machine.Status.NodeRef != nil {
				updatedReadyReplicas++
			}
			continue
		}
		outdatedMachines = append(outdatedMachines, machine)
	}

	// Rolling update behavior when machines are outdated
	if len(outdatedMachines) > 0 {
		if currentReplicas < desiredReplicas+maxSurge {
			nextIndex := r.nextMachineIndex(machines, kcp.Name)
			if err := r.createControlPlaneMachine(ctx, log, kcp, cluster, nextIndex); err != nil {
				return fmt.Errorf("failed to create control plane machine during rollout: %w", err)
			}
			return nil
		}

		// If we are above desired replicas and have enough updated/ready replicas, delete one outdated machine
		if currentReplicas > desiredReplicas && updatedReadyReplicas >= desiredReplicas {
			target := outdatedMachines[0]
			log.Info("Deleting outdated control plane machine", "machine", target.Name)
			if err := r.Delete(ctx, target); err != nil {
				return fmt.Errorf("failed to delete outdated control plane machine: %w", err)
			}
			return nil
		}
	}

	// Create machines if needed
	if currentReplicas < desiredReplicas {
		toCreate := desiredReplicas - currentReplicas
		if toCreate > 0 {
			nextIndex := r.nextMachineIndex(machines, kcp.Name)
			if err := r.createControlPlaneMachine(ctx, log, kcp, cluster, nextIndex); err != nil {
				return fmt.Errorf("failed to create control plane machine: %w", err)
			}
			// Only create one per reconcile to avoid over-scaling
			return nil
		}
	}

	// Delete machines if needed (scale down)
	if currentReplicas > desiredReplicas {
		target := r.selectMachineForDeletion(machines, outdatedMachines)
		if target != nil {
			log.Info("Scaling down control plane machine", "machine", target.Name)
			if err := r.Delete(ctx, target); err != nil {
				return fmt.Errorf("failed to delete control plane machine: %w", err)
			}
		}
	}

	return nil
}

func (r *KairosControlPlaneReconciler) createControlPlaneMachine(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, index int32) error {
	machineName := fmt.Sprintf("%s-%d", kcp.Name, index)

	// Create KairosConfig
	distribution := kcp.Spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}
	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", kcp.Name, index),
			Namespace: kcp.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         cluster.Name,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      distribution,
			KubernetesVersion: kcp.Spec.Version,
		},
	}

	// Determine single-node mode from replicas
	replicas := int32(1)
	if kcp.Spec.Replicas != nil {
		replicas = *kcp.Spec.Replicas
	}
	kairosConfig.Spec.SingleNode = (replicas == 1)
	log.Info("Setting SingleNode flag", "singleNode", kairosConfig.Spec.SingleNode, "replicas", replicas)

	// If there's a template, merge its spec
	if kcp.Spec.KairosConfigTemplate.Name != "" {
		template := &bootstrapv1beta2.KairosConfigTemplate{}
		templateKey := types.NamespacedName{
			Namespace: kcp.Namespace,
			Name:      kcp.Spec.KairosConfigTemplate.Name,
		}
		if err := r.Get(ctx, templateKey, template); err != nil {
			return fmt.Errorf("failed to get KairosConfigTemplate: %w", err)
		}
		// Merge template spec
		kairosConfig.Spec = template.Spec.Template.Spec
		kairosConfig.Spec.Role = "control-plane"
		kairosConfig.Spec.Distribution = distribution
		kairosConfig.Spec.KubernetesVersion = kcp.Spec.Version
		// Override SingleNode based on replicas (replicas takes precedence)
		kairosConfig.Spec.SingleNode = (replicas == 1)
	}

	if err := r.Create(ctx, kairosConfig); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	// Create infrastructure machine (clone from template)
	infraMachine, err := r.createInfrastructureMachine(ctx, log, kcp, cluster, machineName)
	if err != nil {
		return fmt.Errorf("failed to create infrastructure machine: %w", err)
	}

	// Create Machine
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: kcp.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         cluster.Name,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: cluster.Name,
			Version:     &kcp.Spec.Version,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					APIVersion: bootstrapv1beta2.GroupVersion.String(),
					Kind:       "KairosConfig",
					Name:       kairosConfig.Name,
					Namespace:  kairosConfig.Namespace,
				},
			},
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: infraMachine.GetObjectKind().GroupVersionKind().GroupVersion().String(),
				Kind:       infraMachine.GetObjectKind().GroupVersionKind().Kind,
				Name:       infraMachine.GetName(),
				Namespace:  infraMachine.GetNamespace(),
			},
		},
	}

	return r.Create(ctx, machine)
}

func (r *KairosControlPlaneReconciler) createInfrastructureMachine(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machineName string) (client.Object, error) {
	infraRef := kcp.Spec.MachineTemplate.InfrastructureRef

	// Prepare labels and annotations
	labels := map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}
	// Merge with template metadata labels
	if kcp.Spec.MachineTemplate.Metadata.Labels != nil {
		for k, v := range kcp.Spec.MachineTemplate.Metadata.Labels {
			labels[k] = v
		}
	}

	annotations := map[string]string{}
	// Merge with template metadata annotations
	if kcp.Spec.MachineTemplate.Metadata.Annotations != nil {
		for k, v := range kcp.Spec.MachineTemplate.Metadata.Annotations {
			annotations[k] = v
		}
	}

	// Clone infrastructure machine using the helper
	infraMachine, err := infrastructure.CloneInfrastructureMachine(
		ctx,
		r.Client,
		r.Scheme,
		infraRef,
		machineName,
		kcp.Namespace,
		labels,
		annotations,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to clone infrastructure machine: %w", err)
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(kcp, infraMachine, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create the infrastructure machine
	if err := r.Create(ctx, infraMachine); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create infrastructure machine: %w", err)
		}
		// Machine already exists, get it
		if err := r.Get(ctx, types.NamespacedName{Name: machineName, Namespace: kcp.Namespace}, infraMachine); err != nil {
			return nil, fmt.Errorf("failed to get existing infrastructure machine: %w", err)
		}
	}

	log.Info("Created infrastructure machine", "kind", infraRef.Kind, "name", machineName)
	return infraMachine, nil
}

func (r *KairosControlPlaneReconciler) getControlPlaneMachines(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) ([]*clusterv1.Machine, error) {
	selector := labels.SelectorFromSet(map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	})

	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(kcp.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}

	machines := make([]*clusterv1.Machine, 0, len(machineList.Items))
	for i := range machineList.Items {
		machine := &machineList.Items[i]
		// Check if this machine is owned by this KCP
		ownerRef := metav1.GetControllerOf(machine)
		if ownerRef != nil && ownerRef.Kind == "KairosControlPlane" && ownerRef.Name == kcp.Name {
			machines = append(machines, machine)
		}
	}

	return machines, nil
}

func (r *KairosControlPlaneReconciler) machineMatchesVersion(machine *clusterv1.Machine, desiredVersion string) bool {
	if machine.Spec.Version == nil {
		return false
	}
	return *machine.Spec.Version == desiredVersion
}

func (r *KairosControlPlaneReconciler) nextMachineIndex(machines []*clusterv1.Machine, kcpName string) int32 {
	prefix := fmt.Sprintf("%s-", kcpName)
	maxIndex := int32(-1)
	for _, machine := range machines {
		if !strings.HasPrefix(machine.Name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(machine.Name, prefix)
		if suffix == "" {
			continue
		}
		if idx, err := strconv.Atoi(suffix); err == nil {
			if int32(idx) > maxIndex {
				maxIndex = int32(idx)
			}
		}
	}
	return maxIndex + 1
}

func (r *KairosControlPlaneReconciler) selectMachineForDeletion(machines []*clusterv1.Machine, outdatedMachines []*clusterv1.Machine) *clusterv1.Machine {
	if len(outdatedMachines) > 0 {
		return outdatedMachines[0]
	}
	if len(machines) == 0 {
		return nil
	}
	// Delete the newest machine for scale down to reduce churn on older nodes
	return machines[len(machines)-1]
}

func (r *KairosControlPlaneReconciler) updateStatus(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return err
	}

	kcp.Status.Replicas = int32(len(machines))

	readyReplicas := int32(0)
	updatedReplicas := int32(0)
	unavailableReplicas := int32(0)

	for _, machine := range machines {
		// Check if machine is ready (has NodeRef)
		if machine.Status.NodeRef != nil {
			readyReplicas++
		}

		// Check if machine is updated (matches desired version)
		if machine.Spec.Version != nil && *machine.Spec.Version == kcp.Spec.Version {
			updatedReplicas++
		}

		// Check if machine is unavailable
		if machine.Status.Phase != string(clusterv1.MachinePhaseRunning) {
			unavailableReplicas++
		}
	}

	// ReadyReplicas should only be counted when NodeRef is actually set
	// This ensures the Cluster controller can properly evaluate control plane readiness
	// We do NOT artificially count machines as ready replicas without NodeRef, as this
	// creates reconcile loops and confuses the Cluster controller

	// Always set ReadyReplicas, even when it's 0, to ensure it's not null in the API
	// The Cluster controller checks this field, and null vs 0 can cause issues
	kcp.Status.ReadyReplicas = readyReplicas
	kcp.Status.UpdatedReplicas = updatedReplicas
	kcp.Status.UnavailableReplicas = unavailableReplicas

	selector := labels.SelectorFromSet(map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	})
	kcp.Status.Selector = selector.String()

	// Log status field updates for debugging
	log.Info("Updated control plane status fields",
		"readyReplicas", readyReplicas,
		"updatedReplicas", updatedReplicas,
		"unavailableReplicas", unavailableReplicas,
		"replicas", kcp.Status.Replicas)

	// Mark as initialized if we have at least one ready replica (NodeRef set)
	// OR if kubeconfig exists (control plane is functional even without NodeRef)
	// The Cluster controller checks status.Initialized to set ControlPlaneInitialized condition
	// Note: We set Initialized=true when kubeconfig exists to allow the Machine controller
	// to connect and set NodeRef, even if ReadyReplicas is still 0
	if readyReplicas > 0 && !kcp.Status.Initialized {
		kcp.Status.Initialized = true
		log.Info("Control plane initialized (NodeRef set)", "readyReplicas", readyReplicas)
	} else if readyReplicas == 0 && !kcp.Status.Initialized {
		// Check if kubeconfig exists - if so, mark as initialized even without NodeRef
		// This allows the Machine controller to connect and set NodeRef
		secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
		secretKey := types.NamespacedName{
			Name:      secretName,
			Namespace: cluster.Namespace,
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, secretKey, secret); err == nil {
			if kubeconfig, ok := secret.Data["value"]; ok && len(kubeconfig) > 0 {
				kcp.Status.Initialized = true
				log.Info("Control plane initialized (kubeconfig exists, NodeRef pending)", "readyReplicas", readyReplicas)
			}
		}
	} else if kcp.Status.Initialized && readyReplicas > 0 {
		// Ensure Initialized stays true when we have ready replicas
		// This handles the case where Initialized was set early (via kubeconfig)
		// and now we have NodeRef set
		log.V(4).Info("Control plane already initialized, readyReplicas confirmed", "readyReplicas", readyReplicas)
	}

	// Set initialization.controlPlaneInitialized for the CAPI v1beta2 contract.
	// This field is used by the Cluster controller to set ControlPlaneInitialized.
	if kcp.Status.Initialization.ControlPlaneInitialized == nil || *kcp.Status.Initialization.ControlPlaneInitialized != kcp.Status.Initialized {
		initialized := kcp.Status.Initialized
		kcp.Status.Initialization.ControlPlaneInitialized = &initialized
		log.V(4).Info("Updated control plane initialization status",
			"controlPlaneInitialized", initialized)
	}

	return nil
}

// reconcileKubeconfig retrieves the kubeconfig from the control plane node and stores it in a secret
func (r *KairosControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	// Check if kubeconfig secret already exists
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}

	existingSecret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, existingSecret); err == nil {
		// Secret already exists, check if it's valid
		if kubeconfig, ok := existingSecret.Data["value"]; ok && len(kubeconfig) > 0 {
			if updated, err := r.ensureKubeconfigSecretMetadata(ctx, existingSecret, cluster); err != nil {
				return err
			} else if updated {
				log.Info("Updated kubeconfig secret metadata", "secret", secretName)
			}
			log.V(4).Info("Kubeconfig secret already exists", "secret", secretName)
			return nil
		}
	}

	// Get the first ready control plane machine
	// Prefer machines with NodeRef, but fallback to infrastructure-ready machines
	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return fmt.Errorf("failed to get control plane machines: %w", err)
	}

	var readyMachine *clusterv1.Machine
	var fallbackMachine *clusterv1.Machine

	for _, machine := range machines {
		// Prefer machines with NodeRef (fully registered)
		if machine.Status.NodeRef != nil {
			readyMachine = machine
			break
		}
		// Fallback: use infrastructure-ready machine even without NodeRef
		// This allows kubeconfig retrieval when k0s is running but node hasn't registered yet
		if fallbackMachine == nil &&
			conditions.IsTrue(machine, clusterv1.InfrastructureReadyCondition) &&
			conditions.IsTrue(machine, clusterv1.BootstrapReadyCondition) {
			fallbackMachine = machine
		}
	}

	// Use fallback if no machine with NodeRef found
	if readyMachine == nil {
		readyMachine = fallbackMachine
	}

	if readyMachine == nil {
		return fmt.Errorf("no ready control plane machine found (checked NodeRef and infrastructure readiness)")
	}

	if readyMachine.Status.NodeRef != nil {
		log.Info("Found ready control plane machine with NodeRef", "machine", readyMachine.Name, "nodeRef", readyMachine.Status.NodeRef)
	} else {
		log.Info("Found infrastructure-ready control plane machine (no NodeRef yet)", "machine", readyMachine.Name,
			"infrastructureReady", conditions.IsTrue(readyMachine, clusterv1.InfrastructureReadyCondition),
			"bootstrapReady", conditions.IsTrue(readyMachine, clusterv1.BootstrapReadyCondition))
	}

	// For KubeVirt, skip SSH once kubeconfig is present to avoid timeouts in bridged setups.
	if isKubevirtMachine(readyMachine) {
		kubevirtSecret := &corev1.Secret{}
		if err := r.Get(ctx, secretKey, kubevirtSecret); err == nil {
			if kubeconfig, ok := kubevirtSecret.Data["value"]; ok && len(kubeconfig) > 0 {
				log.Info("Skipping SSH kubeconfig retrieval for KubeVirt; secret already exists", "secret", secretName)
				return nil
			}
		}
	}

	// Retrieve kubeconfig from the node
	// For k0s: k0s kubeconfig admin (from /var/lib/k0s/pki/admin.conf)
	// For k3s: cat /etc/rancher/k3s/k3s.yaml
	log.Info("Retrieving kubeconfig from node", "machine", readyMachine.Name, "distribution", kcp.Spec.Distribution)
	kubeconfig, err := r.retrieveKubeconfigFromNode(ctx, log, kcp, readyMachine, cluster)
	if err != nil {
		return fmt.Errorf("failed to retrieve kubeconfig from node: %w", err)
	}

	// k3s default kubeconfig uses https://127.0.0.1:6443 - update to node IP so management cluster can connect
	if kcp.Spec.Distribution == "k3s" {
		nodeIP, nodeErr := r.getNodeIP(ctx, log, readyMachine)
		sshHost, hostErr := resolveSSHHost(readyMachine, cluster, nodeIP, nodeErr, log)
		if hostErr == nil && sshHost != "" {
			updated, updateErr := updateKubeconfigServerToNodeIP(kubeconfig, sshHost, 6443)
			if updateErr != nil {
				log.Error(updateErr, "Failed to update kubeconfig server for k3s, using as-is")
			} else {
				kubeconfig = updated
				log.Info("Updated k3s kubeconfig server to node IP", "nodeIP", sshHost)
			}
		}
	}

	// Create or update the kubeconfig secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{
			"value": kubeconfig,
		},
	}
	if _, err := r.ensureKubeconfigSecretMetadata(ctx, secret, cluster); err != nil {
		return err
	}

	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Update existing secret
			if err := r.Update(ctx, secret); err != nil {
				return fmt.Errorf("failed to update kubeconfig secret: %w", err)
			}
		} else {
			return fmt.Errorf("failed to create kubeconfig secret: %w", err)
		}
	}

	log.Info("Kubeconfig secret created/updated", "secret", secretName)
	return nil
}

// ensureProviderIDOnNodes patches workload cluster Nodes with the Machine providerID.
// This avoids relying on in-VM scripts and allows Machine-to-NodeRef matching.
func (r *KairosControlPlaneReconciler) ensureProviderIDOnNodes(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	kubeconfig, ok := secret.Data["value"]
	if !ok || len(kubeconfig) == 0 {
		return nil
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build workload rest config: %w", err)
	}

	workloadClient, err := client.New(restConfig, client.Options{Scheme: r.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create workload client: %w", err)
	}

	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		return nil
	}

	nodeList := &corev1.NodeList{}
	if err := workloadClient.List(ctx, nodeList); err != nil {
		return fmt.Errorf("failed to list workload nodes: %w", err)
	}

	singleNodeFallback := len(machines) == 1 && len(nodeList.Items) == 1

	for _, machine := range machines {
		providerID := ""
		if machine.Spec.ProviderID != nil {
			providerID = *machine.Spec.ProviderID
		}
		if providerID == "" {
			providerID = r.getInfrastructureProviderID(ctx, log, machine)
		}
		if providerID == "" {
			log.V(4).Info("Skipping providerID patch: no providerID for machine", "machine", machine.Name)
			continue
		}
		if machine.Status.NodeRef != nil {
			continue
		}

		addressSet := map[string]struct{}{}
		for _, addr := range machine.Status.Addresses {
			if addr.Address != "" {
				addressSet[addr.Address] = struct{}{}
			}
		}
		if len(addressSet) == 0 {
			if ip, err := r.getNodeIP(ctx, log, machine); err == nil && ip != "" {
				addressSet[ip] = struct{}{}
			} else if err != nil {
				log.V(4).Info("Failed to get node IP for providerID patch", "machine", machine.Name, "error", err)
			}
		}

		var nodeToPatch *corev1.Node
		for i := range nodeList.Items {
			node := &nodeList.Items[i]
			if len(addressSet) > 0 {
				matches := false
				for _, addr := range node.Status.Addresses {
					if _, ok := addressSet[addr.Address]; ok {
						matches = true
						break
					}
				}
				if !matches {
					continue
				}
			} else if singleNodeFallback {
				// Single-node fallback: when exactly 1 machine and 1 node, match them
				// (e.g. k3s may use different address formats than CAPV reports)
				log.Info("Using single-node fallback to match machine to node", "machine", machine.Name, "node", node.Name)
			} else {
				continue
			}

			if node.Spec.ProviderID == providerID {
				log.V(4).Info("Node already has providerID", "node", node.Name, "providerID", node.Spec.ProviderID)
				break
			}
			// Kubernetes forbids changing providerID once set; only empty -> valid is allowed
			if node.Spec.ProviderID != "" {
				log.V(4).Info("Node already has providerID (immutable), skipping patch",
					"node", node.Name, "existingProviderID", node.Spec.ProviderID, "machineProviderID", providerID)
				break
			}

			nodeToPatch = node
			break
		}

		if nodeToPatch != nil {
			patchBase := nodeToPatch.DeepCopy()
			nodeToPatch.Spec.ProviderID = providerID
			if err := workloadClient.Patch(ctx, nodeToPatch, client.MergeFrom(patchBase)); err != nil {
				return fmt.Errorf("failed to patch node providerID: %w", err)
			}

			log.Info("Patched workload node providerID",
				"node", nodeToPatch.Name,
				"providerID", nodeToPatch.Spec.ProviderID,
				"machine", machine.Name)
		}
	}

	return nil
}

// getInfrastructureProviderID attempts to retrieve providerID from the infrastructure machine object.
func (r *KairosControlPlaneReconciler) getInfrastructureProviderID(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) string {
	if machine == nil || machine.Spec.InfrastructureRef.Kind == "" {
		return ""
	}

	switch machine.Spec.InfrastructureRef.Kind {
	case "VSphereMachine":
		vsphereMachine := &unstructured.Unstructured{}
		vsphereMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		vsphereMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, vsphereMachineKey, vsphereMachine); err != nil {
			log.V(4).Info("Failed to get VSphereMachine for providerID", "machine", machine.Name, "error", err)
			return ""
		}

		if providerID, found, err := unstructured.NestedString(vsphereMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			return providerID
		}
		if vmUUID, found, err := unstructured.NestedString(vsphereMachine.Object, "status", "vmUUID"); err == nil && found && vmUUID != "" {
			return fmt.Sprintf("vsphere://%s", vmUUID)
		}
		if providerID, found, err := unstructured.NestedString(vsphereMachine.Object, "status", "providerID"); err == nil && found && providerID != "" {
			return providerID
		}
	case "KubevirtMachine", "KubeVirtMachine":
		kubevirtMachine := &unstructured.Unstructured{}
		kubevirtMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		kubevirtMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, kubevirtMachineKey, kubevirtMachine); err != nil {
			log.V(4).Info("Failed to get KubevirtMachine for providerID", "machine", machine.Name, "error", err)
			return ""
		}
		if providerID, found, err := unstructured.NestedString(kubevirtMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			return providerID
		}
	case "DockerMachine":
		dockerMachine := &unstructured.Unstructured{}
		dockerMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		dockerMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, dockerMachineKey, dockerMachine); err != nil {
			log.V(4).Info("Failed to get DockerMachine for providerID", "machine", machine.Name, "error", err)
			return ""
		}
		if providerID, found, err := unstructured.NestedString(dockerMachine.Object, "spec", "providerID"); err == nil && found && providerID != "" {
			return providerID
		}
	}

	return ""
}

// retrieveKubeconfigFromNode retrieves the kubeconfig from a control plane node
// For k0s: runs `k0s kubeconfig admin` (from /var/lib/k0s/pki/admin.conf)
// For k3s: runs `cat /etc/rancher/k3s/k3s.yaml`
func (r *KairosControlPlaneReconciler) retrieveKubeconfigFromNode(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, machine *clusterv1.Machine, cluster *clusterv1.Cluster) ([]byte, error) {
	// Get node IP from infrastructure provider
	nodeIP, nodeErr := r.getNodeIP(ctx, log, machine)
	sshHost, err := resolveSSHHost(machine, cluster, nodeIP, nodeErr, log)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve SSH host: %w", err)
	}

	log.Info("Retrieving kubeconfig from node", "nodeIP", sshHost, "distribution", kcp.Spec.Distribution)

	// Get SSH credentials from KairosConfig
	userName, userPassword, err := r.getSSHCredentials(ctx, log, machine)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH credentials: %w", err)
	}

	distribution := kcp.Spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}

	var kubeconfig []byte
	switch distribution {
	case "k3s":
		kubeconfig, err = r.executeK3sKubeconfigCommand(ctx, log, sshHost, userName, userPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to execute k3s kubeconfig command: %w", err)
		}
	default:
		kubeconfig, err = r.executeK0sKubeconfigCommand(ctx, log, sshHost, userName, userPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to execute k0s kubeconfig command: %w", err)
		}
	}

	log.Info("Successfully retrieved kubeconfig", "nodeIP", sshHost, "distribution", distribution, "kubeconfigSize", len(kubeconfig))
	return kubeconfig, nil
}

// getNodeIP retrieves the node IP from the infrastructure provider.
// Supports CAPD (DockerMachine), CAPV (VSphereMachine/VSphereVM), and CAPK (KubevirtMachine).
func (r *KairosControlPlaneReconciler) getNodeIP(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, error) {
	switch machine.Spec.InfrastructureRef.Kind {
	case "VSphereMachine":
		// First, try to get IP from VSphereMachine status
		vsphereMachine := &unstructured.Unstructured{}
		vsphereMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		vsphereMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereMachineKey, vsphereMachine); err != nil {
			return "", fmt.Errorf("failed to get VSphereMachine: %w", err)
		}

		// Try to get IP from VSphereMachine status.addresses
		if ip := r.extractIPFromUnstructured(vsphereMachine); ip != "" {
			return ip, nil
		}

		// Fallback: try VSphereVM (CAPV creates VSphereVM with the same name)
		vsphereVM := &unstructured.Unstructured{}
		vsphereVM.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "infrastructure.cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "VSphereVM",
		})
		vsphereVMKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereVMKey, vsphereVM); err != nil {
			return "", fmt.Errorf("failed to get VSphereVM: %w", err)
		}

		if ip := r.extractIPFromUnstructured(vsphereVM); ip != "" {
			return ip, nil
		}

		return "", fmt.Errorf("no IP address found in VSphereMachine or VSphereVM status")
	case "KubevirtMachine", "KubeVirtMachine":
		kubevirtMachine := &unstructured.Unstructured{}
		kubevirtMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		kubevirtMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, kubevirtMachineKey, kubevirtMachine); err != nil {
			return "", fmt.Errorf("failed to get KubevirtMachine: %w", err)
		}

		if ip := r.extractIPFromUnstructured(kubevirtMachine); ip != "" {
			return ip, nil
		}

		if ip, err := r.getKubevirtVMIIP(ctx, log, machine); err == nil && ip != "" {
			log.Info("Resolved KubeVirt VMI IP", "machine", machine.Name, "ip", ip)
			return ip, nil
		}

		return "", fmt.Errorf("no IP address found in KubevirtMachine status")
	case "DockerMachine":
		dockerMachine := &unstructured.Unstructured{}
		dockerMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		dockerMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, dockerMachineKey, dockerMachine); err != nil {
			return "", fmt.Errorf("failed to get DockerMachine: %w", err)
		}
		if ip := r.extractIPFromUnstructured(dockerMachine); ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("no IP address found in DockerMachine status")
	default:
		return "", fmt.Errorf("unsupported infrastructure provider: %s", machine.Spec.InfrastructureRef.Kind)
	}
}

// extractIPFromUnstructured extracts IP address from an unstructured object's status
func (r *KairosControlPlaneReconciler) extractIPFromUnstructured(obj *unstructured.Unstructured) string {

	// Extract IP from status.addresses
	// VSphere status structure: status.addresses[].address or status.network[].ipAddrs[]
	addresses, found, err := unstructured.NestedSlice(obj.Object, "status", "addresses")
	if err == nil && found && len(addresses) > 0 {
		// Try to get IP from addresses array
		// Prefer InternalIP, then ExternalIP, then any address
		var internalIP, externalIP, anyIP string
		for _, addr := range addresses {
			if addrMap, ok := addr.(map[string]interface{}); ok {
				addrType, _ := addrMap["type"].(string)
				if ip, ok := addrMap["address"].(string); ok && ip != "" {
					switch addrType {
					case "InternalIP":
						internalIP = ip
					case "ExternalIP":
						externalIP = ip
					default:
						if anyIP == "" {
							anyIP = ip
						}
					}
				}
			}
		}
		// Return in priority order: InternalIP > ExternalIP > any IP
		if internalIP != "" {
			return internalIP
		}
		if externalIP != "" {
			return externalIP
		}
		if anyIP != "" {
			return anyIP
		}
	}

	// Fallback: try status.network[].ipAddrs[]
	network, found, err := unstructured.NestedSlice(obj.Object, "status", "network")
	if err == nil && found && len(network) > 0 {
		for _, net := range network {
			if netMap, ok := net.(map[string]interface{}); ok {
				if ipAddrs, ok := netMap["ipAddrs"].([]interface{}); ok && len(ipAddrs) > 0 {
					if ip, ok := ipAddrs[0].(string); ok && ip != "" {
						return ip
					}
				}
			}
		}
	}

	// Also check if there's a direct IP in status (some CAPV versions)
	if ip, found, err := unstructured.NestedString(obj.Object, "status", "vmIp"); err == nil && found && ip != "" {
		return ip
	}

	return ""
}

func (r *KairosControlPlaneReconciler) getKubevirtVMIIP(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, error) {
	if machine == nil {
		return "", fmt.Errorf("machine is nil")
	}

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "VirtualMachineInstance",
	})
	vmiKey := types.NamespacedName{
		Name:      machine.Spec.InfrastructureRef.Name,
		Namespace: machine.Spec.InfrastructureRef.Namespace,
	}
	if err := r.Get(ctx, vmiKey, vmi); err != nil {
		return "", fmt.Errorf("failed to get VMI: %w", err)
	}

	interfaces, found, err := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	if err != nil || !found {
		return "", fmt.Errorf("VMI interfaces not found")
	}

	for _, iface := range interfaces {
		if ifaceMap, ok := iface.(map[string]interface{}); ok {
			if ip, ok := ifaceMap["ipAddress"].(string); ok && ip != "" {
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("no IP address found in VMI status.interfaces")
}

func resolveSSHHost(machine *clusterv1.Machine, cluster *clusterv1.Cluster, nodeIP string, nodeErr error, log logr.Logger) (string, error) {
	if nodeIP != "" {
		return nodeIP, nil
	}

	if isKubevirtMachine(machine) && cluster != nil {
		fallbackHost := cluster.Spec.ControlPlaneEndpoint.Host
		if isValidEndpointHost(fallbackHost) {
			log.Info("Using controlPlaneEndpoint host for KubeVirt SSH fallback", "host", fallbackHost, "machine", machine.Name)
			return fallbackHost, nil
		}
	}

	if nodeErr != nil {
		return "", nodeErr
	}

	return "", fmt.Errorf("node IP not available yet")
}

func isKubevirtMachine(machine *clusterv1.Machine) bool {
	if machine == nil {
		return false
	}

	kind := machine.Spec.InfrastructureRef.Kind
	return kind == "KubevirtMachine" || kind == "KubeVirtMachine"
}

func isKubevirtControlPlane(kcp *controlplanev1beta2.KairosControlPlane) bool {
	if kcp == nil {
		return false
	}
	kind := kcp.Spec.MachineTemplate.InfrastructureRef.Kind
	return kind == "KubevirtMachineTemplate" || kind == "KubeVirtMachineTemplate"
}

func controlPlaneLBServiceName(clusterName string) string {
	return fmt.Sprintf("%s-%s", clusterName, controlPlaneLBServiceSuffix)
}

func isValidEndpointHost(host string) bool {
	return host != "" && host != "0.0.0.0" && host != "::"
}

// getSSHCredentials retrieves SSH credentials from KairosConfig
func (r *KairosControlPlaneReconciler) getSSHCredentials(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, string, error) {
	if machine.Spec.Bootstrap.ConfigRef == nil {
		return "", "", fmt.Errorf("machine has no bootstrap config ref")
	}

	// Get KairosConfig
	kairosConfig := &bootstrapv1beta2.KairosConfig{}
	kairosConfigKey := types.NamespacedName{
		Name:      machine.Spec.Bootstrap.ConfigRef.Name,
		Namespace: machine.Spec.Bootstrap.ConfigRef.Namespace,
	}

	if err := r.Get(ctx, kairosConfigKey, kairosConfig); err != nil {
		return "", "", fmt.Errorf("failed to get KairosConfig: %w", err)
	}

	// Get username and password from spec
	userName := kairosConfig.Spec.UserName
	if userName == "" {
		userName = "kairos" // Default
	}

	userPassword := kairosConfig.Spec.UserPassword
	if userPassword == "" {
		userPassword = "kairos" // Default
	}

	log.V(4).Info("Retrieved SSH credentials", "userName", userName)
	return userName, userPassword, nil
}

// checkK0sReady checks if k0s is ready by verifying the service is running and admin.conf exists
func (r *KairosControlPlaneReconciler) checkK0sReady(ctx context.Context, log logr.Logger, client *ssh.Client) error {
	// Check if k0s service is running
	checkCommands := []string{
		"sudo -n systemctl is-active k0scontroller || sudo -n systemctl is-active k0s",
		"sudo systemctl is-active k0scontroller || sudo systemctl is-active k0s",
		"systemctl is-active k0scontroller || systemctl is-active k0s",
	}

	for _, cmd := range checkCommands {
		session, err := client.NewSession()
		if err != nil {
			continue
		}

		var stdout, stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr

		err = session.Run(cmd)
		session.Close()

		if err == nil {
			output := stdout.String()
			if output == "active\n" || output == "active" {
				log.V(4).Info("k0s service is active")
				// Also check if admin.conf exists
				checkFileCmd := "sudo -n test -f /var/lib/k0s/pki/admin.conf || sudo test -f /var/lib/k0s/pki/admin.conf || test -f /var/lib/k0s/pki/admin.conf"
				fileSession, err := client.NewSession()
				if err == nil {
					err = fileSession.Run(checkFileCmd)
					fileSession.Close()
					if err == nil {
						log.V(4).Info("k0s admin.conf file exists")
						return nil
					}
				}
				// Service is active but admin.conf doesn't exist yet - k0s is still initializing
				log.V(4).Info("k0s service is active but admin.conf not found yet, k0s may still be initializing")
				return fmt.Errorf("k0s is running but admin.conf not found yet - k0s may still be initializing")
			}
		}
	}

	return fmt.Errorf("k0s service is not active")
}

// executeK0sKubeconfigCommand SSHes into the node and runs `k0s kubeconfig admin`
func (r *KairosControlPlaneReconciler) executeK0sKubeconfigCommand(ctx context.Context, log logr.Logger, nodeIP, userName, userPassword string) ([]byte, error) {
	// Create SSH client config
	config := &ssh.ClientConfig{
		User: userName,
		Auth: []ssh.AuthMethod{
			ssh.Password(userPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // In production, use proper host key verification
		Timeout:         30 * time.Second,
	}

	// Connect to the node
	address := net.JoinHostPort(nodeIP, "22")
	log.V(4).Info("Connecting to node via SSH", "address", address)

	client, err := ssh.Dial("tcp", address, config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial SSH: %w", err)
	}
	defer client.Close()

	// Check if k0s is ready before attempting to get kubeconfig
	if err := r.checkK0sReady(ctx, log, client); err != nil {
		return nil, fmt.Errorf("k0s is not ready yet: %w", err)
	}

	// Create a session
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Run k0s kubeconfig admin command with sudo
	// k0s kubeconfig admin typically requires root/sudo access to read the kubeconfig
	// Use timeout context to avoid hanging
	commandCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Try with sudo first (non-interactive), fallback to direct command if sudo is not available
	// sudo -n (non-interactive) will fail if password is required, but won't hang
	commands := []string{
		"sudo -n k0s kubeconfig admin", // Non-interactive sudo (requires passwordless sudo)
		"sudo k0s kubeconfig admin",    // Interactive sudo (may prompt for password)
		"k0s kubeconfig admin",         // Direct command (if user has permissions)
	}

	var output []byte
	var lastErr error

	for _, cmd := range commands {
		log.V(4).Info("Trying kubeconfig command", "command", cmd)

		// Create a new session for each attempt
		session, err := client.NewSession()
		if err != nil {
			return nil, fmt.Errorf("failed to create SSH session: %w", err)
		}

		// Capture both stdout and stderr
		var stdout, stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr

		// Execute command with timeout
		done := make(chan error, 1)

		go func() {
			err := session.Run(cmd)
			done <- err
		}()

		select {
		case err := <-done:
			session.Close()
			if err == nil {
				output = stdout.Bytes()
				if len(output) > 0 {
					log.Info("Successfully retrieved kubeconfig", "command", cmd, "size", len(output))
					return output, nil
				}
			} else {
				stderrStr := stderr.String()
				lastErr = fmt.Errorf("command '%s' failed: %w, stderr: %s", cmd, err, stderrStr)
				log.Info("Command failed, trying next", "command", cmd, "error", err, "stderr", stderrStr)
			}
		case <-commandCtx.Done():
			session.Close()
			lastErr = fmt.Errorf("timeout waiting for kubeconfig command: %s", cmd)
			log.V(4).Info("Command timed out, trying next", "command", cmd)
		}
	}

	// All commands failed
	if lastErr != nil {
		return nil, fmt.Errorf("all kubeconfig commands failed, last error: %w. Ensure the user has sudo access or k0s is accessible without sudo", lastErr)
	}

	return nil, fmt.Errorf("k0s kubeconfig command returned empty output")
}

// checkK3sReady checks if k3s is ready by verifying the service is running and k3s.yaml exists
func (r *KairosControlPlaneReconciler) checkK3sReady(ctx context.Context, log logr.Logger, client *ssh.Client) error {
	// Check if k3s service is running (k3s server uses "k3s" service name)
	checkCommands := []string{
		"sudo -n systemctl is-active k3s",
		"sudo systemctl is-active k3s",
		"systemctl is-active k3s",
	}

	for _, cmd := range checkCommands {
		session, err := client.NewSession()
		if err != nil {
			continue
		}

		var stdout, stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr

		err = session.Run(cmd)
		session.Close()

		if err == nil {
			output := stdout.String()
			if output == "active\n" || output == "active" {
				log.V(4).Info("k3s service is active")
				// Also check if k3s.yaml exists
				checkFileCmd := "sudo -n test -f /etc/rancher/k3s/k3s.yaml || sudo test -f /etc/rancher/k3s/k3s.yaml || test -f /etc/rancher/k3s/k3s.yaml"
				fileSession, err := client.NewSession()
				if err == nil {
					err = fileSession.Run(checkFileCmd)
					fileSession.Close()
					if err == nil {
						log.V(4).Info("k3s kubeconfig file exists")
						return nil
					}
				}
				log.V(4).Info("k3s service is active but k3s.yaml not found yet, k3s may still be initializing")
				return fmt.Errorf("k3s is running but k3s.yaml not found yet - k3s may still be initializing")
			}
		}
	}

	return fmt.Errorf("k3s service is not active")
}

// executeK3sKubeconfigCommand SSHes into the node and runs `cat /etc/rancher/k3s/k3s.yaml`
func (r *KairosControlPlaneReconciler) executeK3sKubeconfigCommand(ctx context.Context, log logr.Logger, nodeIP, userName, userPassword string) ([]byte, error) {
	config := &ssh.ClientConfig{
		User: userName,
		Auth: []ssh.AuthMethod{
			ssh.Password(userPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	address := net.JoinHostPort(nodeIP, "22")
	log.V(4).Info("Connecting to node via SSH", "address", address)

	client, err := ssh.Dial("tcp", address, config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial SSH: %w", err)
	}
	defer client.Close()

	if err := r.checkK3sReady(ctx, log, client); err != nil {
		return nil, fmt.Errorf("k3s is not ready yet: %w", err)
	}

	// k3s kubeconfig is at /etc/rancher/k3s/k3s.yaml
	commands := []string{
		"sudo -n cat /etc/rancher/k3s/k3s.yaml",
		"sudo cat /etc/rancher/k3s/k3s.yaml",
		"cat /etc/rancher/k3s/k3s.yaml",
	}

	commandCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var output []byte
	var lastErr error

	for _, cmd := range commands {
		log.V(4).Info("Trying k3s kubeconfig command", "command", cmd)

		session, err := client.NewSession()
		if err != nil {
			return nil, fmt.Errorf("failed to create SSH session: %w", err)
		}

		var stdout, stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr

		done := make(chan error, 1)
		go func() {
			done <- session.Run(cmd)
		}()

		select {
		case err := <-done:
			session.Close()
			if err == nil {
				output = stdout.Bytes()
				if len(output) > 0 {
					log.Info("Successfully retrieved k3s kubeconfig", "command", cmd, "size", len(output))
					return output, nil
				}
			} else {
				lastErr = fmt.Errorf("command '%s' failed: %w, stderr: %s", cmd, err, stderr.String())
				log.V(4).Info("Command failed, trying next", "command", cmd, "error", err)
			}
		case <-commandCtx.Done():
			session.Close()
			lastErr = fmt.Errorf("timeout waiting for kubeconfig command: %s", cmd)
			log.V(4).Info("Command timed out, trying next", "command", cmd)
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all k3s kubeconfig commands failed, last error: %w", lastErr)
	}
	return nil, fmt.Errorf("k3s kubeconfig command returned empty output")
}

// updateClusterStatus updates the Cluster status based on control plane readiness
func (r *KairosControlPlaneReconciler) updateClusterStatus(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	// Check if kubeconfig secret exists
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Kubeconfig not ready yet - don't update cluster status, just return
			// The cluster controller will handle status updates
			log.V(4).Info("Kubeconfig secret not found, skipping cluster status update", "secret", secretName)
			return nil
		}
		return err
	}

	log.Info("updateClusterStatus called", "cluster", cluster.Name, "kubeconfigExists", true)

	// Re-fetch the cluster to ensure we have the latest version before updating
	// This prevents conflicts with other controllers that might be updating the cluster
	clusterKey := types.NamespacedName{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
	}
	clusterToPatch := &clusterv1.Cluster{}
	if err := r.Get(ctx, clusterKey, clusterToPatch); err != nil {
		return fmt.Errorf("failed to re-fetch cluster for updating: %w", err)
	}

	// Set controlPlaneEndpoint if not already set
	// This is required for the Machine controller to connect to the workload cluster
	// Track if we need to update the spec
	needsSpecUpdate := false
	currentHost := clusterToPatch.Spec.ControlPlaneEndpoint.Host
	currentPort := clusterToPatch.Spec.ControlPlaneEndpoint.Port
	log.V(4).Info("Checking controlPlaneEndpoint", "cluster", clusterToPatch.Name, "currentHost", currentHost, "currentPort", currentPort)

	if isKubevirtControlPlane(kcp) {
		lbHost, lbPort, err := r.getControlPlaneLBEndpoint(ctx, log, clusterToPatch)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get control plane LoadBalancer endpoint", "cluster", clusterToPatch.Name)
		}
		if lbHost != "" && lbPort != 0 {
			shouldUpdate := currentHost == "" || currentPort == 0 || currentHost != lbHost || currentPort != lbPort
			if shouldUpdate {
				clusterToPatch.Spec.ControlPlaneEndpoint.Host = lbHost
				clusterToPatch.Spec.ControlPlaneEndpoint.Port = lbPort
				needsSpecUpdate = true
				log.Info("Setting controlPlaneEndpoint from LoadBalancer", "cluster", clusterToPatch.Name, "host", lbHost, "port", lbPort)
			} else {
				log.V(4).Info("controlPlaneEndpoint already set to LoadBalancer", "currentHost", currentHost, "currentPort", currentPort)
			}

			updated, err := r.ensureKubeconfigServer(ctx, log, secret, lbHost, lbPort)
			if err != nil {
				log.Error(err, "Failed to ensure kubeconfig server", "cluster", clusterToPatch.Name)
			} else if updated {
				log.Info("Updated kubeconfig server to match LoadBalancer endpoint", "cluster", clusterToPatch.Name, "host", lbHost, "port", lbPort)
			}
		} else {
			log.Info("LoadBalancer endpoint not ready yet", "cluster", clusterToPatch.Name)
		}
	} else {
		machines, err := r.getControlPlaneMachines(ctx, kcp, clusterToPatch)
		if err != nil {
			log.V(4).Info("Failed to get machines", "error", err)
		} else if len(machines) == 0 {
			log.V(4).Info("No machines found")
		} else {
			log.V(4).Info("Found machines", "count", len(machines))
			// Find the first machine with an IP address
			// Try machine.Status.Addresses first, then fallback to VSphereMachine/VSphereVM
			for _, machine := range machines {
				log.V(4).Info("Checking machine", "machine", machine.Name, "addressCount", len(machine.Status.Addresses))
				var controlPlaneAddress string

				// First, try machine.Status.Addresses (if populated)
				if len(machine.Status.Addresses) > 0 {
					var controlPlaneIP string
					var controlPlaneHostname string
					for _, addr := range machine.Status.Addresses {
						log.V(4).Info("Machine address", "machine", machine.Name, "type", addr.Type, "address", addr.Address)
						if addr.Type == clusterv1.MachineExternalIP || addr.Type == clusterv1.MachineInternalIP {
							controlPlaneIP = addr.Address
						}
						if addr.Type == clusterv1.MachineInternalDNS {
							controlPlaneHostname = addr.Address
						}
					}
					// Prefer IP address, fallback to hostname
					controlPlaneAddress = controlPlaneIP
					if controlPlaneAddress == "" && controlPlaneHostname != "" {
						controlPlaneAddress = controlPlaneHostname
						log.V(4).Info("Using hostname from machine status", "hostname", controlPlaneHostname)
					}
				}

				// Fallback: Get IP from infrastructure provider (same method used for kubeconfig)
				if controlPlaneAddress == "" {
					log.V(4).Info("Machine.Status.Addresses empty, trying infrastructure provider", "machine", machine.Name)
					if ip, err := r.getNodeIP(ctx, log, machine); err == nil && ip != "" {
						controlPlaneAddress = ip
						log.V(4).Info("Found IP from infrastructure provider", "machine", machine.Name, "ip", ip)
					} else if err != nil {
						log.V(4).Info("Failed to get IP from infrastructure provider", "machine", machine.Name, "error", err)
					}
				}

				if controlPlaneAddress != "" {
					shouldUpdate := currentHost == "" || currentPort == 0 || currentHost != controlPlaneAddress
					if shouldUpdate {
						clusterToPatch.Spec.ControlPlaneEndpoint.Host = controlPlaneAddress
						clusterToPatch.Spec.ControlPlaneEndpoint.Port = 6443 // Default k0s API server port
						needsSpecUpdate = true
						log.Info("Setting controlPlaneEndpoint", "cluster", clusterToPatch.Name, "host", controlPlaneAddress, "port", 6443)
						break
					}
					log.V(4).Info("controlPlaneEndpoint already set", "currentHost", currentHost, "currentPort", currentPort)
				} else {
					log.V(4).Info("No IP or hostname found for machine", "machine", machine.Name)
				}
			}
		}
	}

	// The Cluster API Cluster controller manages the ControlPlaneInitialized condition
	// based on the control plane's status.Initialized field.
	// We should NOT try to set this condition directly, as it causes reconcile loops
	// and conflicts with the Cluster controller's logic.
	// Instead, we ensure status.Initialized is set correctly on the KCP resource,
	// and let the Cluster controller manage the condition on the Cluster resource.

	// Update spec first if needed (controlPlaneEndpoint)
	if needsSpecUpdate {
		log.Info("Updating cluster spec with controlPlaneEndpoint", "cluster", clusterToPatch.Name, "host", clusterToPatch.Spec.ControlPlaneEndpoint.Host, "port", clusterToPatch.Spec.ControlPlaneEndpoint.Port)
		// Use Update() for spec changes
		if err := r.Update(ctx, clusterToPatch); err != nil {
			if apierrors.IsConflict(err) {
				log.V(4).Info("Conflict updating cluster spec, will retry on next reconcile", "cluster", clusterToPatch.Name, "error", err)
				return nil // Will retry on next reconcile
			}
			return fmt.Errorf("failed to update cluster spec: %w", err)
		}
		log.Info("Successfully updated cluster spec with controlPlaneEndpoint", "cluster", clusterToPatch.Name)
		// Re-fetch after spec update to ensure we have latest version
		if err := r.Get(ctx, client.ObjectKeyFromObject(clusterToPatch), clusterToPatch); err != nil {
			return fmt.Errorf("failed to re-fetch cluster after spec update: %w", err)
		}
	}

	// Note: We do NOT update Cluster status conditions here.
	// The Cluster controller manages ControlPlaneInitialized and ControlPlaneReady conditions
	// based on the control plane's status.Initialized and ReadyReplicas fields.
	// Attempting to set these conditions directly causes reconcile loops and conflicts.

	return nil
}

func (r *KairosControlPlaneReconciler) reconcileControlPlaneLB(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	if cluster == nil {
		return nil
	}

	service, err := r.ensureControlPlaneLBService(ctx, log, kcp, cluster)
	if err != nil {
		return err
	}

	if err := r.ensureControlPlaneLBEndpoints(ctx, log, kcp, cluster, service.Name); err != nil {
		return err
	}

	return nil
}

func (r *KairosControlPlaneReconciler) ensureControlPlaneLBService(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (*corev1.Service, error) {
	serviceName := controlPlaneLBServiceName(cluster.Name)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: cluster.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.Labels == nil {
			service.Labels = map[string]string{}
		}
		service.Labels[clusterv1.ClusterNameLabel] = cluster.Name

		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		service.Spec.Selector = nil
		service.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "k8s-api",
				Protocol:   corev1.ProtocolTCP,
				Port:       6443,
				TargetPort: intstr.FromInt(6443),
			},
		}

		return controllerutil.SetControllerReference(kcp, service, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure control plane LoadBalancer service: %w", err)
	}

	return service, nil
}

func (r *KairosControlPlaneReconciler) ensureControlPlaneLBEndpoints(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, serviceName string) error {
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: cluster.Namespace,
		},
	}

	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return fmt.Errorf("failed to list control plane machines: %w", err)
	}

	addresses := make([]corev1.EndpointAddress, 0, len(machines))
	for _, machine := range machines {
		if !isKubevirtMachine(machine) {
			continue
		}
		ip, err := r.getKubevirtVMIIP(ctx, log, machine)
		if err != nil || ip == "" {
			log.V(4).Info("No VMI IP yet for control plane endpoint", "machine", machine.Name, "error", err)
			continue
		}
		addresses = append(addresses, corev1.EndpointAddress{IP: ip})
	}

	subsets := []corev1.EndpointSubset{}
	if len(addresses) > 0 {
		subsets = []corev1.EndpointSubset{
			{
				Addresses: addresses,
				Ports: []corev1.EndpointPort{
					{
						Name:     "k8s-api",
						Port:     6443,
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		}
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, endpoints, func() error {
		if endpoints.Labels == nil {
			endpoints.Labels = map[string]string{}
		}
		endpoints.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		endpoints.Subsets = subsets
		return controllerutil.SetControllerReference(kcp, endpoints, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to ensure control plane endpoints: %w", err)
	}

	return nil
}

func (r *KairosControlPlaneReconciler) getControlPlaneLBEndpoint(ctx context.Context, log logr.Logger, cluster *clusterv1.Cluster) (string, int32, error) {
	serviceName := controlPlaneLBServiceName(cluster.Name)
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: cluster.Namespace}, service); err != nil {
		return "", 0, err
	}

	host := ""
	if len(service.Status.LoadBalancer.Ingress) > 0 {
		ingress := service.Status.LoadBalancer.Ingress[0]
		if ingress.IP != "" {
			host = ingress.IP
		} else if ingress.Hostname != "" {
			host = ingress.Hostname
		}
	}
	if host == "" && len(service.Spec.ExternalIPs) > 0 {
		host = service.Spec.ExternalIPs[0]
	}

	port := int32(6443)
	for _, svcPort := range service.Spec.Ports {
		if svcPort.Port != 0 {
			port = svcPort.Port
			break
		}
	}

	return host, port, nil
}

// updateKubeconfigServerToNodeIP replaces 127.0.0.1/localhost in kubeconfig server with the node IP.
// k3s default kubeconfig uses https://127.0.0.1:6443 which is not reachable from the management cluster.
func updateKubeconfigServerToNodeIP(kubeconfig []byte, nodeIP string, port int32) ([]byte, error) {
	if nodeIP == "" || port == 0 {
		return kubeconfig, nil
	}
	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}
	desired := fmt.Sprintf("https://%s:%d", nodeIP, port)
	changed := false
	for _, cluster := range config.Clusters {
		if cluster == nil {
			continue
		}
		parsed, err := url.Parse(cluster.Server)
		if err != nil {
			cluster.Server = desired
			changed = true
			continue
		}
		// Replace 127.0.0.1 or localhost with node IP
		if parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" {
			cluster.Server = desired
			changed = true
		}
	}
	if !changed {
		return kubeconfig, nil
	}
	out, err := clientcmd.Write(*config)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}
	return out, nil
}

// ensureKubeconfigServer updates the kubeconfig secret server to match the given endpoint.
func (r *KairosControlPlaneReconciler) ensureKubeconfigServer(ctx context.Context, log logr.Logger, secret *corev1.Secret, host string, port int32) (bool, error) {
	if host == "" || port == 0 {
		return false, nil
	}

	kubeconfig, ok := secret.Data["value"]
	if !ok || len(kubeconfig) == 0 {
		return false, nil
	}

	if updated, err := r.ensureKubeconfigSecretMetadata(ctx, secret, nil); err != nil {
		return false, err
	} else if updated {
		log.Info("Updated kubeconfig secret metadata", "secret", secret.Name)
	}

	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return false, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	desired := fmt.Sprintf("https://%s:%d", host, port)
	changed := false
	for _, cluster := range config.Clusters {
		if cluster == nil {
			continue
		}
		if cluster.Server == "" {
			cluster.Server = desired
			changed = true
			continue
		}
		parsed, err := url.Parse(cluster.Server)
		if err != nil {
			cluster.Server = desired
			changed = true
			continue
		}
		if parsed.Host != fmt.Sprintf("%s:%d", host, port) {
			cluster.Server = desired
			changed = true
		}
	}

	if !changed {
		return false, nil
	}

	out, err := clientcmd.Write(*config)
	if err != nil {
		return false, fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}

	secretCopy := secret.DeepCopy()
	secretCopy.Data["value"] = out
	if err := r.Update(ctx, secretCopy); err != nil {
		return false, fmt.Errorf("failed to update kubeconfig secret: %w", err)
	}

	return true, nil
}

func (r *KairosControlPlaneReconciler) ensureKubeconfigSecretMetadata(ctx context.Context, secret *corev1.Secret, cluster *clusterv1.Cluster) (bool, error) {
	changed := false
	secretCopy := secret.DeepCopy()

	if secretCopy.Labels == nil {
		secretCopy.Labels = map[string]string{}
	}
	if cluster != nil {
		if secretCopy.Labels[clusterv1.ClusterNameLabel] != cluster.Name {
			secretCopy.Labels[clusterv1.ClusterNameLabel] = cluster.Name
			changed = true
		}
	}

	if cluster != nil {
		ownerRef := metav1.OwnerReference{
			APIVersion: cluster.APIVersion,
			Kind:       cluster.Kind,
			Name:       cluster.Name,
			UID:        cluster.UID,
			Controller: func() *bool { b := true; return &b }(),
		}
		hasOwner := false
		for _, ref := range secretCopy.OwnerReferences {
			if ref.UID == ownerRef.UID {
				hasOwner = true
				break
			}
		}
		if !hasOwner {
			secretCopy.OwnerReferences = append(secretCopy.OwnerReferences, ownerRef)
			changed = true
		}
	}

	if !changed {
		return false, nil
	}
	if secretCopy.ResourceVersion == "" {
		*secret = *secretCopy
		return true, nil
	}
	if err := r.Update(ctx, secretCopy); err != nil {
		return false, fmt.Errorf("failed to update kubeconfig secret metadata: %w", err)
	}
	return true, nil
}

// triggerClusterReconciliation updates a Cluster annotation to trigger Cluster controller reconciliation
// This is needed because the Cluster controller watches Cluster resources, not ControlPlane resources directly.
// When KCP status.Initialized changes, we update the Cluster annotation to ensure the Cluster controller
// reconciles promptly and sets the ControlPlaneInitialized condition.
func (r *KairosControlPlaneReconciler) triggerClusterReconciliation(ctx context.Context, log logr.Logger, cluster *clusterv1.Cluster) error {
	if cluster == nil {
		return fmt.Errorf("cluster is nil")
	}

	// Re-fetch the cluster to ensure we have the latest version
	clusterKey := types.NamespacedName{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
	}
	clusterToUpdate := &clusterv1.Cluster{}
	if err := r.Get(ctx, clusterKey, clusterToUpdate); err != nil {
		return fmt.Errorf("failed to re-fetch cluster: %w", err)
	}

	// Update annotation with current timestamp to trigger reconciliation
	// The Cluster controller watches Cluster resources, so any change triggers reconciliation
	annotationKey := "controlplane.cluster.x-k8s.io/status-initialized-timestamp"
	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Only update if annotation doesn't exist or has changed
	// This prevents unnecessary updates and potential reconcile loops
	if clusterToUpdate.Annotations == nil {
		clusterToUpdate.Annotations = make(map[string]string)
	}
	currentTimestamp := clusterToUpdate.Annotations[annotationKey]
	if currentTimestamp == timestamp {
		// Already set to current timestamp, no need to update
		return nil
	}

	clusterToUpdate.Annotations[annotationKey] = timestamp
	if err := r.Update(ctx, clusterToUpdate); err != nil {
		if apierrors.IsConflict(err) {
			// Conflict is fine - Cluster controller is reconciling, which is what we want
			log.V(4).Info("Conflict updating Cluster annotation (expected), Cluster controller is reconciling", "cluster", cluster.Name)
			return nil
		}
		return fmt.Errorf("failed to update Cluster annotation: %w", err)
	}

	log.V(4).Info("Triggered Cluster reconciliation via annotation", "cluster", cluster.Name, "annotation", annotationKey)
	return nil
}

func (r *KairosControlPlaneReconciler) reconcileDelete(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) (ctrl.Result, error) {
	// Remove finalizer
	controllerutil.RemoveFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer)
	return ctrl.Result{}, r.Update(ctx, kcp)
}

// SetupWithManager sets up the controller with the Manager.
func (r *KairosControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1beta2.KairosControlPlane{}).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.machineToKairosControlPlane),
		).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToKairosControlPlane),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToKairosControlPlane),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return strings.HasSuffix(obj.GetName(), "-kubeconfig")
			})),
		).
		Complete(r)
}

// machineToKairosControlPlane maps a Machine to its KairosControlPlane
func (r *KairosControlPlaneReconciler) machineToKairosControlPlane(ctx context.Context, o client.Object) []reconcile.Request {
	machine, ok := o.(*clusterv1.Machine)
	if !ok {
		return nil
	}

	// Check if it's a control plane machine
	if !util.IsControlPlaneMachine(machine) {
		return nil
	}

	// Find the owning KairosControlPlane
	ownerRef := metav1.GetControllerOf(machine)
	if ownerRef == nil {
		return nil
	}

	if ownerRef.Kind != "KairosControlPlane" || ownerRef.APIVersion != controlplanev1beta2.GroupVersion.String() {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      ownerRef.Name,
				Namespace: machine.Namespace,
			},
		},
	}
}

// clusterToKairosControlPlane maps a Cluster to its KairosControlPlane
func (r *KairosControlPlaneReconciler) clusterToKairosControlPlane(ctx context.Context, o client.Object) []reconcile.Request {
	cluster, ok := o.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	if cluster.Spec.ControlPlaneRef == nil {
		return nil
	}

	if cluster.Spec.ControlPlaneRef.Kind != "KairosControlPlane" {
		return nil
	}

	// Check API version/group matches
	// In v1beta2, ControlPlaneRef uses apiGroup in YAML, but Go type uses APIVersion
	refAPIVersion := cluster.Spec.ControlPlaneRef.APIVersion
	expectedGroup := controlplanev1beta2.GroupVersion.Group
	expectedVersion := controlplanev1beta2.GroupVersion.String()

	// Match if APIVersion is empty (v1beta2 using apiGroup), matches expected version, or contains expected group
	if refAPIVersion != "" &&
		refAPIVersion != expectedVersion &&
		!(len(refAPIVersion) > 0 && len(expectedGroup) > 0 && refAPIVersion[:len(expectedGroup)] == expectedGroup) {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      cluster.Spec.ControlPlaneRef.Name,
				Namespace: cluster.Namespace,
			},
		},
	}
}

// secretToKairosControlPlane maps a kubeconfig Secret to its KairosControlPlane.
// This ensures KCP reconciles when kubeconfig is pushed from the VM.
func (r *KairosControlPlaneReconciler) secretToKairosControlPlane(ctx context.Context, o client.Object) []reconcile.Request {
	secret, ok := o.(*corev1.Secret)
	if !ok {
		return nil
	}

	if !strings.HasSuffix(secret.Name, "-kubeconfig") {
		return nil
	}

	clusterName := strings.TrimSuffix(secret.Name, "-kubeconfig")
	if clusterName == "" {
		return nil
	}

	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: secret.Namespace}, cluster); err != nil {
		return nil
	}

	if cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != "KairosControlPlane" {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      cluster.Spec.ControlPlaneRef.Name,
				Namespace: secret.Namespace,
			},
		},
	}
}
