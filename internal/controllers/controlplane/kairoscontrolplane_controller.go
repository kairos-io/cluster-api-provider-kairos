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
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/infrastructure"
)

// KairosControlPlaneReconciler reconciles a KairosControlPlane object
type KairosControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const controlPlaneLBServiceSuffix = "control-plane-lb"

// kubeconfigReadyTimeout is the elapsed since Status.LastNodePushObserved
// after which KubeconfigReadyCondition's severity escalates from Info to
// Warning. The timeout is not a terminal — past it, the controller still
// waits on the Secret watch. Operator visibility (condition severity)
// changes; no controller-side recovery action. PR-9's SSHFallback opt-in
// is the recovery surface.
//
// 10 minutes covers normal boot/network-init time on lab-grade VMs with
// margin for kairos package downloads; tightening on infra with faster
// boot lands as a per-infra-provider override in a follow-up.
const kubeconfigReadyTimeout = 10 * time.Minute

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
//
// Scope-limit: this PR (KD-14) introduces a deferred patch.Helper.Patch that
// ONLY fires on the early-exit paths (no-Cluster, delete). The hot path
// continues to issue r.Status().Update directly because it relies on
// Update-not-Patch semantics for zero-valued status fields (see the
// "Status().Update() vs Patch()" comment further down). Unifying the hot path
// behind the patch helper is tracked as KD-37 and lands on
// refactor/kcp-patch-helper-unify post-alpha-2.
//
// Named returns (result, retErr) are required so the deferred Patch closure
// can combine its error into retErr via errors.Join.
func (r *KairosControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the KairosControlPlane instance
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize patch helper BEFORE any early returns so paths that don't run
	// the hot-path r.Status().Update still flush observedGeneration and
	// condition transitions. (KD-14.)
	patchHelper, err := patch.NewHelper(kcp, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always set observedGeneration so even early-return paths reconcile it.
	kcp.Status.ObservedGeneration = kcp.Generation

	// patchOnExit controls whether the deferred Patch fires. It defaults to
	// false because the hot path issues its own r.Status().Update and a
	// follow-up Patch with a stale resourceVersion would conflict. Every
	// early-exit path that wants the observedGeneration/condition changes
	// persisted MUST set patchOnExit = true immediately before returning.
	// reconcileDelete signals via skipPatch=true when it has already issued
	// a bare r.Update for the terminal finalizer-remove step; in that case
	// the deferred Patch must be bypassed to avoid racing with the
	// apiserver removing the object after the last finalizer drops.
	patchOnExit := false
	defer func() {
		if !patchOnExit {
			return
		}
		if perr := patchHelper.Patch(ctx, kcp); perr != nil {
			retErr = errors.Join(retErr, perr)
		}
	}()

	// Handle deletion. reconcileDelete signals via skipPatch=true when it
	// has already finalized the object via a bare r.Update; otherwise the
	// deferred Patch should flush observedGeneration/conditions on the
	// non-terminal drain-requeue path.
	if !kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		res, skipPatch, derr := r.reconcileDelete(ctx, log, kcp)
		if !skipPatch {
			patchOnExit = true
		}
		return res, derr
	}

	// Add finalizer if needed. Kept as a bare r.Update so this spec write is
	// flushed independently of the deferred Patch (which only runs on early
	// exits). After the bare Update we re-anchor the patch helper against
	// the post-Update in-memory state, then re-apply the observedGeneration
	// mutation so the deferred Patch on early-exit paths still produces a
	// diff. Cleanup is part of KD-37.
	if !controllerutil.ContainsFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer) {
		controllerutil.AddFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer)
		// Snapshot the desired status before the bare Update wipes our local
		// observedGeneration mutation from the patch-helper diff base.
		desiredObservedGeneration := kcp.Status.ObservedGeneration
		if err := r.Update(ctx, kcp); err != nil {
			return ctrl.Result{}, err
		}
		// r.Update rewrote kcp from the server response, clobbering our
		// in-memory Status.ObservedGeneration. Re-create the patch helper
		// against the fresh state, then re-set the field so the deferred
		// Patch still flushes it on early-exit paths.
		patchHelper, err = patch.NewHelper(kcp, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		kcp.Status.ObservedGeneration = desiredObservedGeneration
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
				// Return to trigger a new reconcile with the label set.
				// The r.Update above already persisted the spec change AND
				// bumped resourceVersion; do NOT fire the deferred patch.
				return ctrl.Result{Requeue: true}, nil
			}
		} else {
			log.Error(err, "Failed to get cluster from metadata")
			return ctrl.Result{}, err
		}
	}
	if cluster == nil {
		log.Info("Cluster is not available yet")
		// Flush observedGeneration via the deferred Patch.
		patchOnExit = true
		return ctrl.Result{}, nil
	}

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

	// reconcileMachines returned nil -- clear any latched failure status
	// unconditionally. Maintainer-confirmed decision #3: gating on
	// ReadyReplicas > 0 conflated "API server not ready yet" with "failure
	// observed", forcing operators to manually clear failureReason/Message
	// to unblock orchestration. API readiness is a v1beta2 condition
	// concern, not a failure-fields concern. (KD-14.)
	kcp.Status.FailureReason = ""
	kcp.Status.FailureMessage = ""

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

	// KD-3b: the controller no longer SSHes into the node to fetch the
	// kubeconfig. Instead, the node (CAPK + CAPV today) pushes its
	// kubeconfig as a Secret using a per-cluster ServiceAccount token
	// minted by the bootstrap controller's ManagementEndpointResolver.
	// Here we observe the Secret's presence and reflect it on
	// KubeconfigReadyCondition.
	//
	// The Secret watch (SetupWithManager) wakes us when the node writes
	// the kubeconfig, so no requeue is needed on the missing-Secret path.
	// LastNodePushObserved is anchored on first-miss observation and
	// drives the Info → Warning severity escalation after
	// kubeconfigReadyTimeout.
	kubeconfigReady, err := r.observeKubeconfigSecret(ctx, log, kcp, cluster)
	if err != nil {
		// Hard errors (Get failures other than NotFound) propagate. The
		// missing-Secret case is signalled by kubeconfigReady=false and a
		// nil err.
		return ctrl.Result{}, fmt.Errorf("observe kubeconfig secret: %w", err)
	}
	if !kubeconfigReady {
		log.V(4).Info("Kubeconfig Secret not yet observed; waiting for node push (KD-3b)",
			"cluster", cluster.Name)
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

	// Failure fields were cleared above immediately after reconcileMachines
	// returned nil (KD-14, maintainer-confirmed decision #3). The previous
	// `if ReadyReplicas > 0` gate at this location is intentionally removed.

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

// observeKubeconfigSecret implements the KD-3b node-push-wait pattern.
//
// The controller no longer SSHes into nodes to fetch the kubeconfig (KD-10).
// The node (CAPK / CAPV under KD-3b) writes its kubeconfig as a Secret to
// the management cluster using a per-cluster ServiceAccount token. Here we
// observe that Secret's presence and reflect it on KubeconfigReadyCondition.
//
// Return values:
//   - ready=true when the Secret exists, parses as a valid kubeconfig, and
//     has the cluster-name label. KubeconfigReadyCondition transitions to
//     True; Status.LastNodePushObserved is cleared.
//   - ready=false on a missing or empty Secret. The condition is set to
//     False(WaitingForNodePush). Status.LastNodePushObserved is anchored
//     to now on first observation; once
//     Now - LastNodePushObserved > kubeconfigReadyTimeout the condition
//     severity escalates from Info to Warning. No requeue is issued; the
//     Secret watch wakes us when the node writes the kubeconfig.
//   - err is non-nil only for hard Get failures (apiserver unreachable,
//     RBAC denied). NotFound is the missing-Secret signal, not an error.
//
// Why not parse-validate the kubeconfig in Go: the node-push payload
// always writes a base64 of the actual on-node admin.conf / k3s.yaml.
// The distribution (k0s / k3s) is the appointed writer and is trusted
// to produce a syntactically valid kubeconfig. Parse-validation in the
// controller would duplicate that responsibility without adding
// correctness — if the distribution writes a malformed kubeconfig, the
// right fix is in the cloud-config template, not in a Go parser here.
// This posture is permanent; no future PR is expected to change it.
func (r *KairosControlPlaneReconciler) observeKubeconfigSecret(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (bool, error) {
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, secretKey, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if kubeconfig, ok := secret.Data["value"]; ok && len(kubeconfig) > 0 {
			// Secret present and non-empty: kubeconfig is ready. Use
			// conditions.Set rather than MarkTrue so the Reason is
			// populated for downstream consumers — `MarkTrue` deliberately
			// leaves Reason empty per CAPI convention, but KubeconfigReady
			// is a custom condition where the Reason is the observable
			// signal of which path achieved readiness (node-push, vs.
			// PR-9's SSH fallback once it lands).
			kcp.Status.LastNodePushObserved = nil
			conditions.Set(kcp, &clusterv1.Condition{
				Type:   controlplanev1beta2.KubeconfigReadyCondition,
				Status: corev1.ConditionTrue,
				Reason: controlplanev1beta2.KubeconfigReadyReason,
			})
			log.V(4).Info("Kubeconfig Secret observed",
				"secret", secretKey.String(),
				"size", len(kubeconfig))
			return true, nil
		}
	}

	// Missing or empty Secret. Anchor LastNodePushObserved on first miss so
	// the severity escalation has a stable timestamp; if a previous reconcile
	// already anchored it, keep that timestamp (we're measuring elapsed since
	// first observation, not since last).
	if kcp.Status.LastNodePushObserved == nil {
		now := metav1.Now()
		kcp.Status.LastNodePushObserved = &now
	}
	severity := clusterv1.ConditionSeverityInfo
	message := fmt.Sprintf("Waiting for the workload node to push its kubeconfig as Secret %s/%s.", cluster.Namespace, secretName)
	if elapsed := time.Since(kcp.Status.LastNodePushObserved.Time); elapsed > kubeconfigReadyTimeout {
		severity = clusterv1.ConditionSeverityWarning
		message = fmt.Sprintf("Workload node has not pushed its kubeconfig to %s/%s for %s; check node networking to the management API server.", cluster.Namespace, secretName, elapsed.Round(time.Second))
	}
	conditions.MarkFalse(kcp,
		controlplanev1beta2.KubeconfigReadyCondition,
		controlplanev1beta2.WaitingForNodePushReason,
		severity,
		"%s", message)
	return false, nil
}

// ensureProviderIDOnNodes and getInfrastructureProviderID were deleted in
// PR-8 of the KD-3b sequence. The in-VM cloud-config now owns setting
// Node.Spec.ProviderID (kubelet --provider-id flag for k3s, systemd
// ExecStartPre drop-in, on-VM self-discovery script, and a post-bootstrap
// `kubectl patch` fallback — all rendered by internal/bootstrap/templates).
// The CAPI core Machine controller then matches Node.Spec.ProviderID to
// Machine.Spec.ProviderID to populate Machine.Status.NodeRef. The
// controlplane reconciler no longer needs to perform the patch itself.

// getNodeIP, extractIPFromUnstructured, getKubevirtVMIIP have been relocated
// to infra_lookup.go (PR-8 of the KD-3b sequence).

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

// updateClusterStatus updates the Cluster status based on control plane readiness
func (r *KairosControlPlaneReconciler) updateClusterStatus(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}

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

	// Set controlPlaneEndpoint if not already set (runs before kubeconfig check so LB endpoint
	// is set as soon as LoadBalancer has an IP, enabling SSH retrieval and Machine controller)
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
			// ensureKubeconfigServer runs below, only when secret exists
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

	// Check if kubeconfig secret exists - early exit if not, but controlPlaneEndpoint already updated above
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(4).Info("Kubeconfig secret not found, skipping cluster status update", "secret", secretName)
			// Still update spec if controlPlaneEndpoint was set (e.g. from LB)
			if needsSpecUpdate {
				log.Info("Updating cluster spec with controlPlaneEndpoint", "cluster", clusterToPatch.Name, "host", clusterToPatch.Spec.ControlPlaneEndpoint.Host, "port", clusterToPatch.Spec.ControlPlaneEndpoint.Port)
				if err := r.Update(ctx, clusterToPatch); err != nil {
					if apierrors.IsConflict(err) {
						log.V(4).Info("Conflict updating cluster spec, will retry on next reconcile", "cluster", clusterToPatch.Name, "error", err)
						return nil
					}
					return fmt.Errorf("failed to update cluster spec: %w", err)
				}
				log.Info("Successfully updated cluster spec with controlPlaneEndpoint", "cluster", clusterToPatch.Name)
			}
			return nil
		}
		return err
	}

	log.Info("updateClusterStatus called", "cluster", cluster.Name, "kubeconfigExists", true)

	// For KubeVirt, ensure kubeconfig server URL matches LoadBalancer endpoint (only when secret exists)
	if isKubevirtControlPlane(kcp) {
		lbHost, lbPort, err := r.getControlPlaneLBEndpoint(ctx, log, clusterToPatch)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get control plane LoadBalancer endpoint for kubeconfig", "cluster", clusterToPatch.Name)
		} else if lbHost != "" && lbPort != 0 {
			updated, err := r.ensureKubeconfigServer(ctx, log, secret, lbHost, lbPort)
			if err != nil {
				log.Error(err, "Failed to ensure kubeconfig server", "cluster", clusterToPatch.Name)
			} else if updated {
				log.Info("Updated kubeconfig server to match LoadBalancer endpoint", "cluster", clusterToPatch.Name, "host", lbHost, "port", lbPort)
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

// reconcileDelete drains the CAPI Machines owned by this KairosControlPlane
// before removing the finalizer. Stripping the finalizer with live owned
// Machines still attached causes the parent Cluster to disappear with
// orphaned children, which is the failure mode that produced KD-4: the CAPI
// machine.cluster.x-k8s.io finalizer never cleared because the Machine
// controller could not find its parent Cluster, requiring a manual
// `kubectl patch --type=json` to unblock.
//
// KairosConfig and the infrastructure Machine are NOT deleted directly --
// they cascade via the CAPI Machine's OwnerReferences (KD-11 keeps the
// Machine as the controller of both children). Deleting them here would
// race with the CAPI Machine controller's own delete flow.
//
// Returns (result, skipPatch, err). When skipPatch is true the caller MUST
// bypass the deferred Patch -- this path has already finalized the object
// via a bare r.Update and a follow-up Patch would race with apiserver GC.
func (r *KairosControlPlaneReconciler) reconcileDelete(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) (ctrl.Result, bool, error) {
	remaining, err := drainOwnedMachines(ctx, r.Client, kcp)
	if err != nil {
		log.Error(err, "Failed to drain owned Machines",
			"kcp", kcp.Name, "namespace", kcp.Namespace, "uid", kcp.UID)
		return ctrl.Result{}, false, err
	}
	if remaining > 0 {
		log.Info("Waiting for owned Machines to be reaped before removing KCP finalizer",
			"kcp", kcp.Name, "namespace", kcp.Namespace, "remaining", remaining)
		// Non-terminal requeue: let the deferred Patch flush
		// observedGeneration/conditions while we wait for the drain.
		return ctrl.Result{RequeueAfter: kcpDeleteRequeueAfter}, false, nil
	}

	// Terminal step: remove the finalizer with a bare r.Update. The patch
	// helper is bypassed via skipPatch=true so the deferred closure in
	// Reconcile does not race apiserver GC once the last finalizer drops.
	// (KD-4, per maintainer-confirmed plan.)
	controllerutil.RemoveFinalizer(kcp, controlplanev1beta2.KairosControlPlaneFinalizer)
	if err := r.Update(ctx, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			// Object already gone -- nothing to do.
			return ctrl.Result{}, true, nil
		}
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}

// kcpDeleteRequeueAfter is the polling interval while waiting for owned
// Machines to be reaped during reconcileDelete. Short enough to be
// responsive, long enough to avoid hammering the apiserver while the CAPI
// Machine controller does its work.
const kcpDeleteRequeueAfter = 10 * time.Second

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
			// KD-15-compliant predicate: filter by Type and the
			// cluster-name label, never by name suffix. The previous
			// strings.HasSuffix predicate matched anything-kubeconfig,
			// including unrelated workload-cluster Secrets and any
			// future tenant that happens to suffix with -kubeconfig.
			// The KD-3b push payload always stamps both fields, so a
			// node-pushed kubeconfig Secret reliably matches.
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				secret, ok := obj.(*corev1.Secret)
				if !ok {
					return false
				}
				if secret.Type != clusterv1.ClusterSecretType {
					return false
				}
				return secret.Labels[clusterv1.ClusterNameLabel] != ""
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
//
// KD-15-compliant: the cluster lookup uses the `cluster.x-k8s.io/cluster-name`
// label, not the legacy `strings.TrimSuffix(secret.Name, "-kubeconfig")`
// heuristic that would mis-match user-created secrets named *-kubeconfig.
func (r *KairosControlPlaneReconciler) secretToKairosControlPlane(ctx context.Context, o client.Object) []reconcile.Request {
	secret, ok := o.(*corev1.Secret)
	if !ok {
		return nil
	}

	clusterName := secret.Labels[clusterv1.ClusterNameLabel]
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
