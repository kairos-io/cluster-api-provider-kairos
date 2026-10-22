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

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// NodeHostnameErrors returns why h cannot be a control-plane node hostname,
// or nil when it can. A valid hostname is a lowercase DNS-1123 label (at most
// 63 characters of [a-z0-9-], starting and ending alphanumeric) with no
// template markup.
//
// The KairosControlPlane controller finds a node's k0s EtcdMember, and its
// etcd-leave key, by the rendered hostname (ADR 0015 PR-3). k0s lowercases
// the hostname into the EtcdMember name, the node-side responder keys on the
// hostname as the kernel has it, and Kairos expands "{{ ... }}" in a hostname
// at boot. A hostname outside this shape can therefore name a different
// member on the node than the controller looks up.
func NodeHostnameErrors(h string) []string {
	var errs []string
	if strings.Contains(h, "{{") {
		errs = append(errs, "must not contain template markup ('{{')")
	}
	errs = append(errs, validation.IsDNS1123Label(h)...)
	return errs
}

// ValidNodeHostname reports whether h passes NodeHostnameErrors.
func ValidNodeHostname(h string) bool {
	return len(NodeHostnameErrors(h)) == 0
}

// RenderedHostname returns the hostname the bootstrap renderer gives the node
// of a Machine named machineName: spec.hostname when it is set, else the
// Machine name. The renderer and the KairosControlPlane controller both use
// it, so the name the controller looks a node up by (its k0s EtcdMember, its
// etcd-leave key) cannot drift from the name the node gets (ADR 0015 PR-3).
func RenderedHostname(spec *KairosConfigSpec, machineName string) string {
	if spec != nil && spec.Hostname != "" {
		return spec.Hostname
	}
	return machineName
}
