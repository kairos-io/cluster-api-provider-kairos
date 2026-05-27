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

package v1beta2

import "time"

// KubeconfigReadyTimeout is the elapsed since Status.LastNodePushObserved
// after which KubeconfigReadyCondition's severity escalates from Info to
// Warning. The timeout is not a terminal — past it, the controller still
// waits on the Secret watch (node-push) and/or fires the SSH fallback
// (if SSHFallback.Enabled=true and ActivateAfter has elapsed). Operator
// visibility (condition severity) changes; no controller-side recovery
// action is taken on the basis of this timeout alone.
//
// 10 minutes covers normal boot/network-init time on lab-grade VMs with
// margin for kairos package downloads; tightening on infra with faster
// boot lands as a per-infra-provider override in a follow-up.
//
// This constant lives in the API package (not the controller) because the
// validating webhook needs to enforce a cross-field invariant against it
// (SSHFallback.ActivateAfter must be strictly greater than this value, so
// the fallback fires AFTER the Info → Warning escalation, not before).
// The controller imports it from here; there is one source of truth.
const KubeconfigReadyTimeout = 10 * time.Minute
