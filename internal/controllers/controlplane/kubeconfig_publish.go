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

// This file implements the control-plane controller's kubeconfig PUBLISH path
// (ADR 0011, "Control-plane controller (publish path)"). The management side no
// longer trusts a node-written kubeconfig verbatim (KD-60, the A1/A2 attack
// class): every byte that reaches the contract Secret `<cluster>-kubeconfig`,
// and every workload client we build from it, is S5 output of the pure
// internal/kubeconfig sanitizer — never the raw node bytes.
//
// PR-2 scope (this file): the consume-side hardening. The controller owns a
// staging Secret `<cluster>-kubeconfig-push`, runs a fail-closed steady-state
// check on the contract Secret, adopts a clean legacy contract Secret through
// the sanitizer, pins the CA on a KCP annotation nodes cannot write, and
// neutralizes unsafe/oversized/unparseable/CA-mismatched content EMPTY-IN-PLACE
// (never delete; the KCP manager holds no secrets:delete — maintainer decision,
// ADR 0011 review record 2026-09-25). The node-side template PATCH to staging
// and the Role migrator that stops nodes writing the contract Secret directly
// are PR-3; in the PR-2 mixed window nodes still write the contract Secret and
// the steady-state check adopts/sanitizes it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubeconfig"
)

const (
	// KubeconfigCAPinAnnotation stores, on the KairosControlPlane, the pinned
	// CA fingerprint (see caFingerprint) of the workload kubeconfig. It lives on
	// the KCP — an object nodes cannot write — precisely so a step-7
	// neutralization that empties the contract Secret cannot drop the pin
	// (ADR 0011 step 5). The first accepted publish sets it; every later publish
	// and republish must match it; it is cleared only by an explicit operator
	// re-pin.
	KubeconfigCAPinAnnotation = "controlplane.cluster.x-k8s.io/kubeconfig-ca-pin"

	// KubeconfigRepinAnnotation, set to "true" by an operator on the
	// KairosControlPlane, tells the controller to drop the stored CA pin and
	// re-pin from the next accepted candidate. The controller consumes it once
	// (deleting both this and the pin annotation) and then re-pins. Deleting
	// KubeconfigCAPinAnnotation directly has the same effect. This is the only
	// sanctioned way past a NodePushCAMismatch (ADR 0011 step 5, migration
	// "possible plant: inspect, then re-pin").
	KubeconfigRepinAnnotation = "controlplane.cluster.x-k8s.io/kubeconfig-repin"

	// KubeconfigSourceNodePush is the source-annotation value stamped on a
	// contract Secret whose content came from the node-push staging channel.
	KubeconfigSourceNodePush = "node-push"

	// KubeconfigSourceAdopted is the source-annotation value stamped on a
	// contract Secret the controller adopted from a legacy node-created Secret
	// (ADR 0011 "Adoption").
	KubeconfigSourceAdopted = "adopted"
)

// contractKubeconfigSecretName returns the CAPI contract kubeconfig Secret name
// (`<cluster>-kubeconfig`) — the name CAPI core, CABPK and upstream KCP read.
func contractKubeconfigSecretName(clusterName string) string {
	return clusterName + "-kubeconfig"
}

// stagingKubeconfigSecretName returns the controller-owned staging Secret name
// (`<cluster>-kubeconfig-push`).
func stagingKubeconfigSecretName(clusterName string) string {
	return bootstrapv1beta2.KubeconfigPushSecretName(clusterName)
}

// reconcileKubeconfig is the single owner of the contract Secret's content and
// of KubeconfigReadyCondition (it subsumes the former observeKubeconfigSecret).
// It runs on every reconcile, for every replica count, after machine
// reconciliation and before updateStatus so the initialized check sees S5
// output. It returns whether the contract Secret currently holds a usable,
// owned, canonical kubeconfig (KubeconfigReadyCondition=True).
//
// It never returns a hard error for a rejected/absent kubeconfig: those are
// signalled through ready=false and a condition, so a bad node push cannot wedge
// the reconcile. err is non-nil only for genuine apiserver failures.
func (r *KairosControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (bool, error) {
	// Honor an operator re-pin request before reading the pin, so this pass
	// re-pins from whatever candidate it accepts (ADR 0011 step 5).
	if err := r.consumeRepinAnnotation(ctx, kcp); err != nil {
		return false, err
	}

	// Ensure + repair the staging Secret (create if missing, KCP-owned; revert
	// node metadata tampering). A repair failure is logged, not fatal: the
	// contract-Secret checks below must still run.
	if err := r.ensureStagingKubeconfigSecret(ctx, log, kcp, cluster); err != nil {
		log.Error(err, "Failed to ensure staging kubeconfig secret", "cluster", cluster.Name)
	}

	expectedServer, ok, err := resolveExpectedServer(ctx, r.Client, kcp, cluster)
	if err != nil {
		return false, err
	}
	if !ok {
		// Endpoint not known yet (e.g. CAPK LB not provisioned). We cannot verify
		// a fixed point or force a server, so fall to the waiting path.
		return r.markWaitingForNodePush(kcp, cluster), nil
	}

	contract := &corev1.Secret{}
	contractKey := types.NamespacedName{Name: contractKubeconfigSecretName(cluster.Name), Namespace: cluster.Namespace}
	getErr := r.Get(ctx, contractKey, contract)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return false, fmt.Errorf("get contract kubeconfig secret %s: %w", contractKey, getErr)
	}
	contractExists := getErr == nil
	contractValue := []byte(nil)
	if contractExists {
		contractValue = contract.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey]
	}

	// A present, non-empty contract Secret drives the steady-state check.
	if contractExists && len(contractValue) > 0 {
		return r.reconcileContractKubeconfig(ctx, log, kcp, cluster, contract, expectedServer)
	}

	// Contract absent or empty (never written, or neutralized): try to (re)publish
	// from staging. If staging has nothing publishable, fall to the waiting path so
	// clustercache retry and SSH-fallback eligibility fire.
	published, ready, err := r.publishFromStaging(ctx, log, kcp, cluster, contract, contractExists, expectedServer)
	if err != nil {
		return false, err
	}
	if published {
		return ready, nil
	}
	return r.markWaitingForNodePush(kcp, cluster), nil
}

// reconcileContractKubeconfig implements ADR 0011 step 7 for a present,
// non-empty contract Secret. It fails CLOSED: anything unsafe, oversized,
// unparseable, or CA-mismatched is emptied in place (never deleted); only
// structure-only (S1) or expiry-only (S3) failures on otherwise-clean content
// are condition-only.
func (r *KairosControlPlaneReconciler) reconcileContractKubeconfig(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, expectedServer string) (bool, error) {
	contractValue := contract.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey]

	ownedByKCP := metav1.IsControlledBy(contract, kcp)
	adoptable := !ownedByKCP && contractAdoptable(contract, cluster)
	if !ownedByKCP && !adoptable {
		// Neither KCP-owned nor adoptable: a leftover from a deleted same-name
		// cluster, a pre-Cluster plant, a plant carrying a forged controller
		// ownerRef, or a mislabeled Secret.
		//
		// SF1 (ADR 0011 step 7 ownership-gate, option (b)): the authoritative
		// direction is Cluster -> KCP (ownerRefs are forgeable; the
		// controlPlaneRef is not, and `<cluster>-kubeconfig` is unique per
		// cluster), so when Cluster.spec.controlPlaneRef points at this KCP the
		// KCP is authoritative for that name. Neutralize the Secret in place
		// (never delete) exactly as for an unsafe owned Secret, then recover from
		// staging/SSH — this closes the A1/A2 window for a forged-ownerRef or
		// pre-Cluster plant that would otherwise be left CAPI-core-readable.
		// Reuse WaitingForNodePushReason (no new reason) so SSH-fallback
		// eligibility (G23) stays intact. Mirrors handleContractRejection.
		if clusterControlPlaneRefMatches(cluster, kcp) {
			if err := r.neutralizeContract(ctx, log, kcp, cluster, contract,
				controlplanev1beta2.WaitingForNodePushReason,
				fmt.Sprintf("Contract kubeconfig for %s/%s is not KCP-owned and not adoptable, but the Cluster controlPlaneRef points at this KairosControlPlane; emptied in place to force recovery. The object is retained, not deleted.", cluster.Namespace, cluster.Name)); err != nil {
				return false, err
			}
			published, ready, perr := r.publishFromStaging(ctx, log, kcp, cluster, contract, true, expectedServer)
			if perr != nil {
				return false, perr
			}
			if published {
				return ready, nil
			}
			return false, nil
		}
		// controlPlaneRef does NOT point here: leave the content untouched (we
		// have no authority over it); ask the operator to delete it (ADR 0011
		// migration rule a-c).
		r.markKubeconfigFalse(kcp, controlplanev1beta2.KubeconfigSecretNotOwnedReason,
			fmt.Sprintf("Contract kubeconfig Secret %s/%s is not controlled by this KairosControlPlane and is not adoptable; delete it so the controller can republish.", cluster.Namespace, contract.Name))
		return false, nil
	}

	sanitized, rejErr := kubeconfig.Sanitize(contractValue, cluster.Name, expectedServer)
	if rejErr != nil {
		return r.handleContractRejection(ctx, log, kcp, cluster, contract, expectedServer, rejErr)
	}

	// Content passed S0-S4. Enforce the CA pin.
	candPin, err := caFingerprint(sanitized)
	if err != nil {
		return false, err
	}
	pin := effectiveCAPin(kcp)
	if pin != "" && pin != candPin {
		// A different CA than the one pinned — a possible plant or split-brain
		// joiner (A8). Neutralize and refuse to republish until re-pinned.
		if err := r.neutralizeContract(ctx, log, kcp, cluster, contract, controlplanev1beta2.NodePushCAMismatchReason,
			fmt.Sprintf("Contract kubeconfig for %s/%s presents a CA that does not match the pinned fingerprint; emptied. Inspect for a possible plant, then re-pin via the %s annotation.", cluster.Namespace, cluster.Name, KubeconfigRepinAnnotation)); err != nil {
			return false, err
		}
		return false, nil
	}

	// Persist the pin on first acceptance.
	if pin == "" {
		if err := r.setCAPinAnnotation(ctx, kcp, candPin); err != nil {
			return false, err
		}
	}

	// Bring the content to canonical S5 form if it drifted (legacy content,
	// endpoint change) or adopt it, then report readiness.
	if !ownedByKCP {
		// Adoption: rewrite with S5 output, take controller ownership, stamp source.
		if err := r.writeContract(ctx, log, kcp, cluster, contract, sanitized, adoptionSource(contract), true); err != nil {
			return false, err
		}
		log.Info("Adopted legacy contract kubeconfig Secret", "cluster", cluster.Name, "secret", contract.Name)
	} else if !bytes.Equal(contractValue, sanitized) {
		// Owned but not canonical: rewrite in place with its own S5 output.
		if err := r.writeContract(ctx, log, kcp, cluster, contract, sanitized, contractSource(contract), true); err != nil {
			return false, err
		}
		log.Info("Rewrote contract kubeconfig Secret to canonical form", "cluster", cluster.Name, "secret", contract.Name)
	}

	return r.markKubeconfigReadyOrExpired(kcp, cluster, contract, sanitized), nil
}

// handleContractRejection maps a sanitizer rejection of the contract Secret to
// either neutralization (unsafe/oversized/unparseable) or a condition-only
// signal (structure-only, credential-shape-only). Expiry never reaches here —
// Sanitize does not check expiry (see markKubeconfigReadyOrExpired).
func (r *KairosControlPlaneReconciler) handleContractRejection(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, expectedServer string, rejErr error) (bool, error) {
	_, msg := rejectionReasonAndMessage(rejErr)
	if neutralizesContent(rejErr) {
		if err := r.neutralizeContract(ctx, log, kcp, cluster, contract,
			controlplanev1beta2.WaitingForNodePushReason,
			fmt.Sprintf("Contract kubeconfig for %s/%s was rejected (%s) and emptied to force recovery; the object is retained, not deleted.", cluster.Namespace, cluster.Name, msg)); err != nil {
			return false, err
		}
		// After emptying, try to recover immediately from clean staging.
		published, ready, perr := r.publishFromStaging(ctx, log, kcp, cluster, contract, true, expectedServer)
		if perr != nil {
			return false, perr
		}
		if published {
			return ready, nil
		}
		return false, nil
	}
	// Structure-only or credential-shape-only on otherwise-clean content:
	// condition only, content untouched (ADR 0011 step 7 last bullet).
	r.markKubeconfigFalse(kcp, controlplanev1beta2.NodePushRejectedReason,
		fmt.Sprintf("Contract kubeconfig for %s/%s failed validation (%s); left in place pending correction.", cluster.Namespace, cluster.Name, msg))
	return false, nil
}

// publishFromStaging attempts to publish the staged candidate into the contract
// Secret. It returns published=false (with no condition set) when staging has no
// usable candidate, so the caller can fall through to the waiting path. It sets
// a False condition and returns published=true when a candidate exists but is
// refused (rejected or CA mismatch), so recovery/operator action is surfaced.
func (r *KairosControlPlaneReconciler) publishFromStaging(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, contractExists bool, expectedServer string) (published, ready bool, err error) {
	staging := &corev1.Secret{}
	stagingKey := types.NamespacedName{Name: stagingKubeconfigSecretName(cluster.Name), Namespace: cluster.Namespace}
	if gerr := r.Get(ctx, stagingKey, staging); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get staging kubeconfig secret %s: %w", stagingKey, gerr)
	}
	// Only the controller-owned staging Secret is a trusted candidate source.
	if !metav1.IsControlledBy(staging, kcp) {
		return false, false, nil
	}
	candidate := staging.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey]
	if len(candidate) == 0 {
		return false, false, nil
	}

	sanitized, rejErr := kubeconfig.Sanitize(candidate, cluster.Name, expectedServer)
	if rejErr != nil {
		_, msg := rejectionReasonAndMessage(rejErr)
		r.markKubeconfigFalse(kcp, controlplanev1beta2.NodePushRejectedReason,
			fmt.Sprintf("Staged kubeconfig for %s/%s was rejected (%s); contract Secret left unchanged.", cluster.Namespace, cluster.Name, msg))
		return true, false, nil
	}

	candPin, err := caFingerprint(sanitized)
	if err != nil {
		return false, false, err
	}
	pin := effectiveCAPin(kcp)
	if pin != "" && pin != candPin {
		r.markKubeconfigFalse(kcp, controlplanev1beta2.NodePushCAMismatchReason,
			fmt.Sprintf("Staged kubeconfig for %s/%s presents a CA that does not match the pinned fingerprint; refused. Inspect, then re-pin via the %s annotation.", cluster.Namespace, cluster.Name, KubeconfigRepinAnnotation))
		return true, false, nil
	}
	if pin == "" {
		if err := r.setCAPinAnnotation(ctx, kcp, candPin); err != nil {
			return false, false, err
		}
	}

	if !contractExists {
		contract.Name = contractKubeconfigSecretName(cluster.Name)
		contract.Namespace = cluster.Namespace
	}
	if err := r.writeContract(ctx, log, kcp, cluster, contract, sanitized, stagingSource(staging), contractExists); err != nil {
		return false, false, err
	}
	log.Info("Published contract kubeconfig from staging", "cluster", cluster.Name, "source", stagingSource(staging))
	return true, r.markKubeconfigReadyOrExpired(kcp, cluster, contract, sanitized), nil
}

// writeContract writes sanitized S5 bytes into the contract Secret, setting the
// controller owner (the KCP), the cluster-name label, the type and the source
// annotation. update=true patches an existing object with a resourceVersion
// precondition; update=false Creates it. It NEVER deletes.
func (r *KairosControlPlaneReconciler) writeContract(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, sanitized []byte, source string, update bool) error {
	if !clusterControlPlaneRefMatches(cluster, kcp) {
		// Authoritative direction is Cluster -> KCP (ADR 0011 API/RBAC): never
		// write on a mere ownerReference we might have set ourselves.
		r.markKubeconfigFalse(kcp, controlplanev1beta2.KubeconfigSecretNotOwnedReason,
			fmt.Sprintf("Cluster %s/%s controlPlaneRef does not point at this KairosControlPlane; refusing to write its kubeconfig.", cluster.Namespace, cluster.Name))
		return nil
	}

	base := contract.DeepCopy()
	if contract.Labels == nil {
		contract.Labels = map[string]string{}
	}
	contract.Labels[clusterv1.ClusterNameLabel] = cluster.Name
	if contract.Annotations == nil {
		contract.Annotations = map[string]string{}
	}
	if source != "" {
		contract.Annotations[KubeconfigSourceAnnotation] = source
	}
	contract.Type = clusterv1.ClusterSecretType
	if contract.Data == nil {
		contract.Data = map[string][]byte{}
	}
	contract.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey] = sanitized
	// The KCP is authoritative for this unique name (guard above) and is the sole
	// owner of the contract Secret. Drop a foreign/forged controller ownerRef a
	// plant may carry (SF1) so SetControllerReference — which errors on a
	// pre-existing DIFFERENT controller — can take ownership. Non-controller
	// owners (e.g. the SSH-fallback Cluster owner preserved during adoption) are
	// kept.
	if existing := metav1.GetControllerOfNoCopy(contract); existing != nil && existing.UID != kcp.UID {
		kept := make([]metav1.OwnerReference, 0, len(contract.OwnerReferences))
		for _, o := range contract.OwnerReferences {
			if o.Controller != nil && *o.Controller {
				continue // drop the foreign controller owner
			}
			kept = append(kept, o)
		}
		contract.OwnerReferences = kept
	}
	if err := controllerutil.SetControllerReference(kcp, contract, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference on contract kubeconfig secret: %w", err)
	}

	if update {
		if err := r.Patch(ctx, contract, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("patch contract kubeconfig secret %s/%s: %w", contract.Namespace, contract.Name, err)
		}
		return nil
	}
	if err := r.Create(ctx, contract); err != nil {
		return fmt.Errorf("create contract kubeconfig secret %s/%s: %w", contract.Namespace, contract.Name, err)
	}
	return nil
}

// neutralizeContract empties the contract Secret's value in place via an UPDATE
// with a resourceVersion precondition (never a delete), sets the given False
// reason, and emits a Warning event. It confirms Cluster.spec.controlPlaneRef
// points at this KCP first (ADR 0011 API/RBAC). Empty-in-place is the maintainer
// decision: the KCP manager holds no secrets:delete.
func (r *KairosControlPlaneReconciler) neutralizeContract(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, reason, message string) error {
	if !clusterControlPlaneRefMatches(cluster, kcp) {
		r.markKubeconfigFalse(kcp, controlplanev1beta2.KubeconfigSecretNotOwnedReason,
			fmt.Sprintf("Cluster %s/%s controlPlaneRef does not point at this KairosControlPlane; refusing to modify its kubeconfig.", cluster.Namespace, cluster.Name))
		return nil
	}
	if len(contract.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey]) != 0 {
		base := contract.DeepCopy()
		if contract.Data == nil {
			contract.Data = map[string][]byte{}
		}
		contract.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey] = []byte{}
		if err := r.Patch(ctx, contract, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("neutralize (empty) contract kubeconfig secret %s/%s: %w", contract.Namespace, contract.Name, err)
		}
		log.Info("Neutralized contract kubeconfig Secret in place (emptied value, retained object)",
			"cluster", cluster.Name, "secret", contract.Name, "reason", reason)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, reason, "%s", message)
		}
	}
	r.markKubeconfigFalse(kcp, reason, message)
	return nil
}

// ensureStagingKubeconfigSecret creates the staging Secret if missing (KCP-owned)
// and repairs node tampering on every pass (ADR 0011 step 1): it enforces exactly
// one controller owner (the KCP), removes finalizers, and restores canonical
// labels. It looks the Secret up by NAME and then requires the controller-owner
// UID to be this KCP — never by label. A node-set immutable Secret is a self-DoS
// the operator clears; the controller surfaces it and does not delete.
func (r *KairosControlPlaneReconciler) ensureStagingKubeconfigSecret(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	key := types.NamespacedName{Name: stagingKubeconfigSecretName(cluster.Name), Namespace: cluster.Namespace}
	staging := &corev1.Secret{}
	err := r.Get(ctx, key, staging)
	if apierrors.IsNotFound(err) {
		staging = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:                     cluster.Name,
					bootstrapv1beta2.KubeconfigPushSecretTypeLabel: bootstrapv1beta2.KubeconfigPushSecretTypeValue,
				},
			},
			Type: corev1.SecretTypeOpaque,
		}
		if serr := controllerutil.SetControllerReference(kcp, staging, r.Scheme); serr != nil {
			return fmt.Errorf("set controller reference on staging secret: %w", serr)
		}
		if cerr := r.Create(ctx, staging); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create staging kubeconfig secret %s: %w", key, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get staging kubeconfig secret %s: %w", key, err)
	}

	// Existing: require this KCP to be the controller owner (by UID), never label.
	owner := metav1.GetControllerOfNoCopy(staging)
	if owner == nil || owner.UID != kcp.UID {
		// A foreign owner (or none) on the staging name — a node-planted staging
		// Secret with a forged owner. Refuse to touch it (we do not adopt or
		// delete) and surface an Event; publishFromStaging separately refuses to
		// use it as a candidate (its IsControlledBy check fails). We use an Event
		// rather than the KubeconfigReady condition here so staging-repair
		// anomalies do not clobber the contract-driven condition.
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "StagingKubeconfigNotOwned",
				"Staging Secret %s is not owned by this KairosControlPlane; delete it so the controller can recreate it.", key)
		}
		return nil
	}

	// Node tampering repair: exactly one ownerReference (the KCP), no finalizers,
	// canonical labels. A node-set immutable Secret blocks the repair — surface a
	// self-DoS condition; do not delete.
	if staging.Immutable != nil && *staging.Immutable {
		// A node that marks its own staging Secret immutable DoSes only its own
		// cluster (a self-DoS residual). Surface an Event; the operator clears it
		// by deleting the staging Secret manually. We never delete it ourselves.
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "StagingKubeconfigImmutable",
				"Staging Secret %s was marked immutable (self-DoS); delete it so the controller can recreate it.", key)
		}
		return nil
	}

	base := staging.DeepCopy()
	changed := false
	if len(staging.Finalizers) != 0 {
		staging.Finalizers = nil
		changed = true
	}
	if len(staging.OwnerReferences) != 1 {
		staging.OwnerReferences = nil
		if serr := controllerutil.SetControllerReference(kcp, staging, r.Scheme); serr != nil {
			return fmt.Errorf("repair staging secret owner: %w", serr)
		}
		changed = true
	}
	if staging.Labels == nil {
		staging.Labels = map[string]string{}
	}
	if staging.Labels[clusterv1.ClusterNameLabel] != cluster.Name {
		staging.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		changed = true
	}
	if staging.Labels[bootstrapv1beta2.KubeconfigPushSecretTypeLabel] != bootstrapv1beta2.KubeconfigPushSecretTypeValue {
		staging.Labels[bootstrapv1beta2.KubeconfigPushSecretTypeLabel] = bootstrapv1beta2.KubeconfigPushSecretTypeValue
		changed = true
	}
	if changed {
		if err := r.Patch(ctx, staging, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("repair staging kubeconfig secret %s: %w", key, err)
		}
		log.V(4).Info("Repaired staging kubeconfig Secret metadata", "cluster", cluster.Name, "secret", key.Name)
	}
	return nil
}

// markWaitingForNodePush reproduces the KD-3b waiting behavior for an absent or
// emptied contract Secret: it anchors LastNodePushObserved, defers to the SSH
// fallback sibling once that owns the condition, and escalates the severity from
// Info to Warning past kubeconfigReadyTimeout. It returns false (not ready).
func (r *KairosControlPlaneReconciler) markWaitingForNodePush(kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) bool {
	if kcp.Status.LastNodePushObserved == nil {
		now := metav1.Now()
		kcp.Status.LastNodePushObserved = &now
	}
	// Let the SSH-fallback sibling own the condition once it has taken it over,
	// so the Reason does not flap (see observeKubeconfigSecret's original note).
	if sshFallbackOwnsKubeconfigCondition(kcp) {
		return false
	}
	secretName := contractKubeconfigSecretName(cluster.Name)
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
	return false
}

// markKubeconfigReadyOrExpired sets KubeconfigReadyCondition=True (routing the
// Reason by source annotation) when the sanitized content's client certificate
// is unexpired, or False(KubeconfigCertificateExpired) when it is expired —
// content left in place either way (ADR 0011 step 7: expiry never neutralizes).
// It returns whether the condition is True.
func (r *KairosControlPlaneReconciler) markKubeconfigReadyOrExpired(kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, contract *corev1.Secret, sanitized []byte) bool {
	if err := kubeconfig.CheckClientCertificateExpiry(sanitized, time.Now()); err != nil {
		var rej *kubeconfig.RejectedError
		if errors.As(err, &rej) && rej.Reason == kubeconfig.ReasonExpired {
			r.markKubeconfigFalse(kcp, controlplanev1beta2.KubeconfigCertificateExpiredReason,
				fmt.Sprintf("Contract kubeconfig for %s/%s has an expired client certificate; left in place. %s", cluster.Namespace, cluster.Name, rej.Message))
			return false
		}
		// A non-expiry parse error on our own S5 output should not happen; report
		// it as rejected rather than claim readiness.
		r.markKubeconfigFalse(kcp, controlplanev1beta2.NodePushRejectedReason,
			fmt.Sprintf("Contract kubeconfig for %s/%s failed the publish-time certificate check.", cluster.Namespace, cluster.Name))
		return false
	}
	kcp.Status.LastNodePushObserved = nil
	reason := controlplanev1beta2.KubeconfigReadyReason
	if contractSource(contract) == KubeconfigSourceSSHFallback {
		reason = controlplanev1beta2.KubeconfigReadyViaSSHFallbackReason
	}
	conditions.Set(kcp, &clusterv1.Condition{
		Type:   controlplanev1beta2.KubeconfigReadyCondition,
		Status: corev1.ConditionTrue,
		Reason: reason,
	})
	return true
}

// markKubeconfigFalse marks KubeconfigReadyCondition False(Warning) with the
// given reason and message. The message names the rule/situation, never a byte
// of the kubeconfig (root CLAUDE.md rule 2).
func (r *KairosControlPlaneReconciler) markKubeconfigFalse(kcp *controlplanev1beta2.KairosControlPlane, reason, message string) {
	conditions.MarkFalse(kcp,
		controlplanev1beta2.KubeconfigReadyCondition,
		reason,
		clusterv1.ConditionSeverityWarning,
		"%s", message)
}

// consumeRepinAnnotation, when the operator has set KubeconfigRepinAnnotation to
// "true", drops both the repin flag and the stored CA pin in one metadata patch,
// so this reconcile re-pins from the next accepted candidate (ADR 0011 step 5).
func (r *KairosControlPlaneReconciler) consumeRepinAnnotation(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane) error {
	if kcp.Annotations[KubeconfigRepinAnnotation] != "true" {
		return nil
	}
	base := kcp.DeepCopy()
	delete(kcp.Annotations, KubeconfigRepinAnnotation)
	delete(kcp.Annotations, KubeconfigCAPinAnnotation)
	if err := r.Patch(ctx, kcp, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("consume kubeconfig re-pin annotation: %w", err)
	}
	return nil
}

// setCAPinAnnotation stores the CA fingerprint on the KCP via a metadata
// MergeFrom patch (never mixed into the status write). r.Patch refreshes the
// in-memory resourceVersion so the later Status().Update does not conflict.
func (r *KairosControlPlaneReconciler) setCAPinAnnotation(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, pin string) error {
	base := kcp.DeepCopy()
	if kcp.Annotations == nil {
		kcp.Annotations = map[string]string{}
	}
	kcp.Annotations[KubeconfigCAPinAnnotation] = pin
	if err := r.Patch(ctx, kcp, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("set kubeconfig CA pin annotation: %w", err)
	}
	return nil
}

// effectiveCAPin returns the CA fingerprint pinned on the KCP, or "" if none.
func effectiveCAPin(kcp *controlplanev1beta2.KairosControlPlane) string {
	return kcp.Annotations[KubeconfigCAPinAnnotation]
}

// contractAdoptable reports whether an existing, non-KCP-owned contract Secret
// meets the ADR 0011 adoption criteria a-c (owner/created-after-Cluster/label).
// Criterion d (passes S0-S4 across every entry) is enforced by the caller via
// Sanitize. A controller owner other than this KCP makes it non-adoptable.
func contractAdoptable(contract *corev1.Secret, cluster *clusterv1.Cluster) bool {
	// (a) no controller owner (node-created Secrets have none; SSH-fallback ones
	// have a non-controller Cluster owner).
	if metav1.GetControllerOfNoCopy(contract) != nil {
		return false
	}
	// (b) creationTimestamp not earlier than the Cluster's — rejects leftovers
	// from a deleted same-name cluster and pre-Cluster plants.
	if contract.CreationTimestamp.Before(&cluster.CreationTimestamp) {
		return false
	}
	// (c) cluster-name label equals the Cluster name.
	if contract.Labels[clusterv1.ClusterNameLabel] != cluster.Name {
		return false
	}
	return true
}

// clusterControlPlaneRefMatches confirms Cluster.spec.controlPlaneRef points at
// this KCP by name and kind — the authoritative direction before any rewrite or
// empty (ADR 0011 API/RBAC: ownerRefs can be hand-set).
func clusterControlPlaneRefMatches(cluster *clusterv1.Cluster, kcp *controlplanev1beta2.KairosControlPlane) bool {
	ref := cluster.Spec.ControlPlaneRef
	if !ref.IsDefined() {
		return false
	}
	if ref.Kind != "KairosControlPlane" || ref.Name != kcp.Name {
		return false
	}
	if ref.APIGroup != "" && ref.APIGroup != controlplanev1beta2.GroupVersion.Group {
		return false
	}
	return true
}

// contractSource returns the source annotation on a contract Secret.
func contractSource(contract *corev1.Secret) string {
	return contract.Annotations[KubeconfigSourceAnnotation]
}

// stagingSource returns the source to stamp on the contract when publishing from
// staging: the staging Secret's own source annotation (ssh-fallback when the SSH
// worker wrote it), defaulting to node-push.
func stagingSource(staging *corev1.Secret) string {
	if s := staging.Annotations[KubeconfigSourceAnnotation]; s != "" {
		return s
	}
	return KubeconfigSourceNodePush
}

// adoptionSource preserves an ssh-fallback provenance during adoption; otherwise
// it stamps `adopted`.
func adoptionSource(contract *corev1.Secret) string {
	if contractSource(contract) == KubeconfigSourceSSHFallback {
		return KubeconfigSourceSSHFallback
	}
	return KubeconfigSourceAdopted
}

// resolveExpectedServer computes the server URL the controller forces into the
// published kubeconfig (ADR 0011 step 2): the CAPK LB Service endpoint, else
// Cluster.spec.controlPlaneEndpoint (port 6443 if zero). ok=false means the
// endpoint is not known yet (requeue/wait), which is not an error.
func resolveExpectedServer(ctx context.Context, c client.Client, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (string, bool, error) {
	if isKubevirtControlPlane(kcp) {
		host, port, err := controlPlaneLBEndpoint(ctx, c, cluster)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, err
		}
		if host == "" || port == 0 {
			return "", false, nil
		}
		return "https://" + net.JoinHostPort(host, strconv.Itoa(int(port))), true, nil
	}
	ep := cluster.Spec.ControlPlaneEndpoint
	if ep.Host == "" {
		return "", false, nil
	}
	port := ep.Port
	if port == 0 {
		port = 6443
	}
	return "https://" + net.JoinHostPort(ep.Host, strconv.Itoa(int(port))), true, nil
}

// caFingerprint returns a stable hex SHA-256 over the DER of each CERTIFICATE in
// the sanitized kubeconfig's CA bundle, in bundle order (ADR 0011 step 5). Input
// is S5 output, whose CA bundle is already canonical, so the fingerprint is
// deterministic.
func caFingerprint(sanitized []byte) (string, error) {
	cfg, err := clientcmd.Load(sanitized)
	if err != nil {
		return "", fmt.Errorf("caFingerprint: load sanitized kubeconfig: %w", err)
	}
	var caData []byte
	for _, cl := range cfg.Clusters {
		if cl != nil && len(cl.CertificateAuthorityData) != 0 {
			caData = cl.CertificateAuthorityData
			break
		}
	}
	if len(caData) == 0 {
		return "", fmt.Errorf("caFingerprint: sanitized kubeconfig has no certificate-authority-data")
	}
	h := sha256.New()
	rest := caData
	blocks := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		h.Write(block.Bytes)
		blocks++
	}
	if blocks == 0 {
		return "", fmt.Errorf("caFingerprint: CA bundle contains no CERTIFICATE blocks")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// trustedWorkloadKubeconfig returns the kubeconfig the KCP may build a
// workload-cluster client from (ADR 0011 step 12, and the SF2 hardening): the
// S5 output of the contract Secret `<cluster>-kubeconfig`, rebuilt with
//
//   - the server the controller resolves itself (resolveExpectedServer: the
//     CAPK LB endpoint, else Cluster.spec.controlPlaneEndpoint), never the
//     server written in the Secret; and
//   - a CA that matches the fingerprint pinned on the KairosControlPlane that
//     Cluster.spec.controlPlaneRef names (KubeconfigCAPinAnnotation). The pin is
//     read from the KCP, which nodes cannot write, never from the Secret.
//
// The KCP is looked up through the Cluster's controlPlaneRef (the authoritative
// Cluster -> KCP direction; ownerRefs are forgeable), so the factory seams that
// only carry the Cluster need no change. Every failure is an error, never a
// fallback to the raw bytes: no endpoint yet, no pin yet, a Sanitize rejection,
// or a CA that does not match the pin. Callers treat the error as fail-safe.
func (r *KairosControlPlaneReconciler) trustedWorkloadKubeconfig(ctx context.Context, cluster *clusterv1.Cluster) ([]byte, error) {
	kcp, err := r.controlPlaneForCluster(ctx, cluster)
	if err != nil {
		return nil, err
	}
	server, ok, err := resolveExpectedServer(ctx, r.Client, kcp, cluster)
	if err != nil {
		return nil, fmt.Errorf("resolve the control-plane endpoint of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	if !ok {
		return nil, fmt.Errorf("the control-plane endpoint of cluster %s/%s is not known yet", cluster.Namespace, cluster.Name)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: contractKubeconfigSecretName(cluster.Name)}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("get workload kubeconfig secret %s: %w", key, err)
	}
	data := secret.Data[bootstrapv1beta2.KubeconfigPushSecretDataKey]
	if len(data) == 0 {
		return nil, fmt.Errorf("workload kubeconfig secret %s has no non-empty 'value' key", key)
	}
	sanitized, err := kubeconfig.Sanitize(data, cluster.Name, server)
	if err != nil {
		// The error names the failing rule, never a byte of the input.
		return nil, fmt.Errorf("workload kubeconfig secret %s failed sanitization: %w", key, err)
	}

	pin := effectiveCAPin(kcp)
	if pin == "" {
		return nil, fmt.Errorf("no CA is pinned on KairosControlPlane %s/%s yet; refusing to build a workload client", kcp.Namespace, kcp.Name)
	}
	got, err := caFingerprint(sanitized)
	if err != nil {
		return nil, err
	}
	if got != pin {
		return nil, fmt.Errorf("workload kubeconfig secret %s presents a CA that does not match the fingerprint pinned on KairosControlPlane %s/%s; refusing to build a workload client", key, kcp.Namespace, kcp.Name)
	}
	return sanitized, nil
}

// controlPlaneForCluster returns the KairosControlPlane that
// Cluster.spec.controlPlaneRef names, in the Cluster's namespace. A ref to any
// other kind is an error: the KCP only builds workload clients for clusters it
// controls.
func (r *KairosControlPlaneReconciler) controlPlaneForCluster(ctx context.Context, cluster *clusterv1.Cluster) (*controlplanev1beta2.KairosControlPlane, error) {
	ref := cluster.Spec.ControlPlaneRef
	if !ref.IsDefined() || ref.Kind != "KairosControlPlane" ||
		(ref.APIGroup != "" && ref.APIGroup != controlplanev1beta2.GroupVersion.Group) {
		return nil, fmt.Errorf("cluster %s/%s does not reference a KairosControlPlane; refusing to build a workload client", cluster.Namespace, cluster.Name)
	}
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: ref.Name}, kcp); err != nil {
		return nil, fmt.Errorf("get KairosControlPlane %s/%s: %w", cluster.Namespace, ref.Name, err)
	}
	return kcp, nil
}

// neutralizesContent reports whether a sanitizer rejection warrants emptying the
// contract Secret (unsafe field, oversized, or unparseable) versus a
// condition-only signal (structure-only, credential-shape-only). Expiry never
// reaches Sanitize, so it is not classified here.
func neutralizesContent(err error) bool {
	var rej *kubeconfig.RejectedError
	if !errors.As(err, &rej) {
		// A non-RejectedError from Sanitize is an internal serialization failure
		// on our own output; treat as non-neutralizing (do not empty on a bug).
		return false
	}
	switch rej.Reason {
	case kubeconfig.ReasonOversized, kubeconfig.ReasonUnparseable,
		kubeconfig.ReasonUnsafeCluster, kubeconfig.ReasonUnsafeIdentity:
		return true
	default: // ReasonStructure, ReasonCredentials
		return false
	}
}

// rejectionReasonAndMessage extracts the sanitizer's structured reason and
// message for logging/conditions. Neither ever contains kubeconfig bytes.
func rejectionReasonAndMessage(err error) (kubeconfig.RejectionReason, string) {
	var rej *kubeconfig.RejectedError
	if errors.As(err, &rej) {
		return rej.Reason, rej.Message
	}
	return "", err.Error()
}
