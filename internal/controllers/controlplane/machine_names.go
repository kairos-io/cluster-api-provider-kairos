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
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Control-plane Machine names (ADR 0015 PR-4, maintainer decision OQ-6).
//
// A control-plane Machine, its KairosConfig and its infrastructure Machine
// share one name, <kcp>-<5 random characters>, as upstream KCP names them. A
// name is never handed to a second Machine. The <kcp>-<index> names this
// replaced gave the next Machine the highest index plus one, so deleting the
// newest Machine handed its name to its replacement. The node name, the etcd
// member name, the etcd-leave ConfigMap key and the etcd-status key all
// default to the Machine name, so a reused name let a joiner meet state its
// predecessor left behind, which on k0s can split etcd (ADR 0015 GT-17, GT-22).
//
// Machines created before this change keep their index names until they are
// replaced. Nothing reads meaning out of a name: Machines are ordered by
// creation time (sortMachinesOldestFirst) and the role of a new Machine
// depends only on how many exist (controlPlaneRoleForNewMachine).
const (
	// controlPlaneMachineNameSuffixLength matches upstream KCP's random suffix.
	controlPlaneMachineNameSuffixLength = 5
	// controlPlaneMachineNamePrefixMax keeps <prefix>-<suffix> within the
	// 63-character hostname limit, since the name is the default hostname.
	controlPlaneMachineNamePrefixMax = 63 - 1 - controlPlaneMachineNameSuffixLength
	// controlPlaneMachineNameAttempts bounds how many fresh names one create
	// tries when a name is already taken.
	controlPlaneMachineNameAttempts = 5
)

// controlPlaneMachineNameSuffix returns the random part of a new control-plane
// Machine name: lowercase consonants and digits, from apimachinery's
// generator, the same one CAPI uses. Names are not secrets, so this is not a
// security primitive. Tests replace it to force collisions.
var controlPlaneMachineNameSuffix = func() string {
	return utilrand.String(controlPlaneMachineNameSuffixLength)
}

// newControlPlaneMachineName returns a fresh name for a control-plane Machine
// of the KCP named kcpName. The name is the node's hostname unless
// spec.hostname is set, so it is always a lowercase DNS-1123 label, the shape
// bootstrapv1beta2.ValidNodeHostname requires (ADR 0015 PR-3): a KCP name is
// a DNS-1123 subdomain, so its dots become dashes, and a long one is cut to
// controlPlaneMachineNamePrefixMax so the result fits in 63 characters. A dash
// left at the end of the cut is dropped. The KCP name starts with an
// alphanumeric, so the prefix is never empty, and the suffix ends with one.
func newControlPlaneMachineName(kcpName string) string {
	prefix := strings.ReplaceAll(kcpName, ".", "-")
	if len(prefix) > controlPlaneMachineNamePrefixMax {
		prefix = strings.TrimRight(prefix[:controlPlaneMachineNamePrefixMax], "-")
	}
	return prefix + "-" + controlPlaneMachineNameSuffix()
}

// errControlPlaneNameTaken reports that an object of a freshly generated name
// already existed. Nothing of this attempt is left behind; the caller tries
// again with a new name.
var errControlPlaneNameTaken = errors.New("control-plane machine name already taken")

// nameTakenError wraps a create's AlreadyExists so that it reads as
// errControlPlaneNameTaken, and keeps the API error for the log.
func nameTakenError(kind, name string, err error) error {
	return fmt.Errorf("%w: %s %s: %w", errControlPlaneNameTaken, kind, name, err)
}

// createdObjects records the objects one create attempt has made, in order, so
// a failure later in the attempt can delete them (as upstream KCP's
// cloneConfigsAndGenerateMachine does with cleanupFromGeneration).
type createdObjects []client.Object

// cleanup deletes every object of the attempt and returns cause, joined with
// any delete that failed. NotFound counts as deleted. When a delete fails the
// result is no longer errControlPlaneNameTaken, so the caller does not try a
// new name while it may be leaving objects behind.
func (c createdObjects) cleanup(ctx context.Context, log logr.Logger, cl client.Client, cause error) error {
	var errs []error
	for i := len(c) - 1; i >= 0; i-- {
		obj := c[i]
		if err := cl.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %T %s/%s left by a failed create: %w", obj, obj.GetNamespace(), obj.GetName(), err))
			continue
		}
		log.Info("Deleted an object left by a failed control-plane machine create", "name", obj.GetName(), "type", fmt.Sprintf("%T", obj))
	}
	if len(errs) == 0 {
		return cause
	}
	return fmt.Errorf("%s; cleanup failed: %w", cause.Error(), kerrors.NewAggregate(errs))
}

// machineCacheWaitBackoff bounds how long createControlPlaneMachine waits for
// a new Machine to show up in the cache: about 10 seconds, as upstream KCP.
// Tests shorten it.
var machineCacheWaitBackoff = wait.Backoff{
	Duration: 25 * time.Microsecond,
	Cap:      2 * time.Second,
	Factor:   1.2,
	Steps:    63,
}

// waitForMachineInCache waits until the client's reads return the Machine just
// created. The next reconcile counts control-plane Machines from the cache;
// if the new one were not there yet, it would create another. The index names
// used to stop that by colliding with the Machine already created; with random
// names this wait is what stops it, as it does in upstream KCP
// (WaitForObjectsToBeAddedToTheCache after createMachine).
func waitForMachineInCache(ctx context.Context, c client.Client, m *clusterv1.Machine) error {
	key := client.ObjectKeyFromObject(m)
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, machineCacheWaitBackoff, func(ctx context.Context) (bool, error) {
		lastErr = c.Get(ctx, key, &clusterv1.Machine{})
		switch {
		case lastErr == nil:
			return true, nil
		case apierrors.IsNotFound(lastErr):
			return false, nil
		default:
			return false, lastErr
		}
	})
	if err != nil {
		if lastErr != nil && !apierrors.IsNotFound(lastErr) {
			err = lastErr
		}
		return fmt.Errorf("control-plane Machine %s was created but is not in the cache yet: %w", key, err)
	}
	return nil
}

// sortMachinesOldestFirst orders control-plane Machines by creation time,
// oldest first. The first one is the init Machine for the join gate, and
// scale-down removes the last one. Creation timestamps have one-second
// resolution, so equal timestamps fall back to the name, which keeps the order
// stable from one reconcile to the next. Two control-plane Machines are never
// created in the same second in practice: the join gate admits one at a time.
func sortMachinesOldestFirst(machines []*clusterv1.Machine) {
	sort.SliceStable(machines, func(i, j int) bool {
		ti, tj := machines[i].CreationTimestamp, machines[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return machines[i].Name < machines[j].Name
	})
}
