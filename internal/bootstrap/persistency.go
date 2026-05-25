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

// SECURITY:
//
//  1. Why is this a compile-time constant?
//     The cloud-config we render runs as root at first boot on every node, and
//     the persistent-state-paths it declares decide which directories survive a
//     reboot on Kairos's immutable rootfs. If a user-controlled string could
//     reach this content, an attacker who controlled a KairosConfig field
//     could either (a) add `/` to PERSISTENT_STATE_PATHS and turn the whole
//     filesystem into mutable state, or (b) inject a yip stage that runs
//     before our bootstrap script. We deliberately keep this content static,
//     package-private, and zero-arg from the template engine so no field on
//     TemplateData can influence it. The cloudconfig-rendering-safety skill
//     calls this out explicitly.
//
//  2. Why /run/cos/extra-layout.env and not /run/cos/cos-layout.env?
//     immucore reads BOTH files when it computes the runtime persistence
//     overlay:
//     - /run/cos/cos-layout.env       (primary, written by Kairos's own
//     00_rootfs.yaml in the base image)
//     - /run/cos/extra-layout.env     (additive, intended for downstream
//     consumers like us)
//     immucore's loader UNIONs PERSISTENT_STATE_PATHS across both files (see
//     immucore steps_shared.go — keys are split on whitespace and deduped).
//     The yip "Environment" plugin under the hood is godotenv, which on a
//     single-file write OVERWRITES values on key collision. If we wrote to
//     cos-layout.env we would either (a) clobber the base-image paths (losing
//     /etc, /var/lib, etc.) or (b) require us to read+merge the existing file
//     ourselves, which is brittle. Writing to extra-layout.env is the
//     contract immucore exposes for downstream additions.
//
//  3. Why these twelve paths and only these twelve?
//     They are exactly the directories the CAPI provider's own cloud-config
//     (in this package's templates) writes to OR depends on after first boot:
//     /etc/cni            — CNI plugin configs (workloads)
//     /etc/k0s            — k0s config + token file (worker join)
//     /etc/kubernetes     — kubeconfigs, manifests staging
//     /etc/rancher        — k3s config + token file (worker join)
//     /etc/ssh            — host keys (avoids MITM on reboot)
//     /etc/systemd        — drop-in units we install (post-bootstrap, lb-sans)
//     /var/lib/cni        — CNI runtime state
//     /var/lib/containerd — container image cache + state
//     /var/lib/k0s        — k0s data dir (etcd-like state, PKI)
//     /var/lib/kubelet    — kubelet state (volumes, pods)
//     /var/lib/rancher    — k3s data dir (etcd-like state, PKI, manifests)
//     /var/log            — journal/log retention across reboot
//     The list is intentionally conservative: only directories this provider
//     touches. We do not add /opt, /home, /root, or application paths — those
//     are user responsibilities, addressable via Spec.Files writing
//     /system/oem/91_*.yaml on a per-cluster basis.
//
//     Adding paths here expands the persistent surface on every cluster. Any
//     edit needs a clear justification.
//
// The const is exported to no one. It is referenced only by the
// `persistencyOEM` template func registered in funcs.go.
const persistencyOEMContent = `name: "Kairos CAPI persistent state paths"
stages:
  rootfs:
    - environment_file: /run/cos/extra-layout.env
      environment:
        PERSISTENT_STATE_PATHS: "/etc/cni /etc/k0s /etc/kubernetes /etc/rancher /etc/ssh /etc/systemd /var/lib/cni /var/lib/containerd /var/lib/k0s /var/lib/kubelet /var/lib/rancher /var/log"
`

// persistencyOEM returns the static OEM cloud-config content that declares the
// persistent-state paths this provider depends on. It takes no arguments — the
// content is intentionally not parameterizable; see the SECURITY notes above.
//
// The returned string is intended to be piped through `indent N` in templates
// so it embeds cleanly under a `content: |` block-scalar.
func persistencyOEM() string {
	return persistencyOEMContent
}
