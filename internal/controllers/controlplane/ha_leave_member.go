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

// The etcd-leave traffic goes to one named member that is staying (ADR 0014).
//
// The control-plane endpoint is the wrong path for it. When the member being
// removed holds the kube-vip VIP, the endpoint is that member's own apiserver,
// and that apiserver loses its etcd backend during the leave. Every read the
// KCP sent through the endpoint then hung, and the leave never finished.
//
// So the KCP reads and writes the leave ConfigMap through the apiserver of a
// member it is keeping, addressed by an IP from that member's Machine. It
// tries the members one at a time, each with a short timeout, and uses the
// first one that answers for the rest of the reconcile. The endpoint client
// is the last resort, for when no member is reachable directly: on CAPK, for
// example, Machines report pod IPs, which are not in the apiserver's
// certificate.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	// k0sAPIServerPort is the port of the kube-apiserver on every k0s
	// controller.
	k0sAPIServerPort = "6443"

	// memberRequestTimeout bounds each request to a single member. A member
	// that does not answer within it is skipped for the next one.
	memberRequestTimeout = 5 * time.Second

	// leaveViaEndpoint names the endpoint client in logs.
	leaveViaEndpoint = "endpoint"
)

// leaveConfigMapKey names the workload-cluster ConfigMap of the handshake.
var leaveConfigMapKey = types.NamespacedName{Namespace: etcdLeaveConfigMapNamespace, Name: etcdLeaveConfigMapName}

// defaultMemberClient builds a client for the kube-apiserver of the member at
// ip. It is used when the reconciler's MemberClientFactory seam is not
// overridden (tests inject a fake).
func (r *KairosControlPlaneReconciler) defaultMemberClient(ctx context.Context, cluster *clusterv1.Cluster, ip string) (client.Client, error) {
	base, err := r.workloadRESTConfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	restCfg, err := memberRESTConfig(base, ip)
	if err != nil {
		return nil, err
	}
	wc, err := newWorkloadClient(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build workload client for member %s of cluster %s/%s: %w", ip, cluster.Namespace, cluster.Name, err)
	}
	return wc, nil
}

// memberRESTConfig points a copy of the workload rest.Config at the
// apiserver of the member at ip, on k0sAPIServerPort. It keeps the
// kubeconfig's cluster CA and credential. The server certificate must be
// signed by that CA and valid for ip itself (k0s puts the node's own
// addresses in it): any ServerName override is cleared so the IP is what is
// checked, and verification is never skipped. Every request is bounded by
// memberRequestTimeout.
func memberRESTConfig(base *rest.Config, ip string) (*rest.Config, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil, fmt.Errorf("member address %q is not an IP address", ip)
	}
	cfg := rest.CopyConfig(base)
	cfg.Host = "https://" + net.JoinHostPort(parsed.String(), k0sAPIServerPort)
	cfg.Timeout = memberRequestTimeout
	cfg.ServerName = ""
	cfg.Insecure = false
	return cfg, nil
}

// leaveCandidateIPs returns the member addresses the etcd-leave traffic for
// target may go to, in the order they are tried.
//
// An address qualifies when it is in status.addresses of exactly one Machine
// in machines, that Machine is neither target nor being deleted, and no
// Machine being deleted also reports it (ownMemberAddresses, ADR 0013). It
// must be a unicast IP that is not loopback or link-local, and it must not be
// one of exclude: the control-plane endpoint host, which the member holding
// the VIP may also report. Addresses of Machines that have a Node come first;
// each group is sorted by address.
func leaveCandidateIPs(machines []*clusterv1.Machine, target *clusterv1.Machine, exclude ...string) []string {
	ours := ownMemberAddresses(machines)
	skip := map[string]bool{}
	for _, h := range exclude {
		if ip := net.ParseIP(h); ip != nil {
			skip[ip.String()] = true
		}
	}
	for _, a := range target.Status.Addresses {
		if ip := net.ParseIP(a.Address); ip != nil {
			skip[ip.String()] = true
		}
	}

	type candidate struct {
		ip         net.IP
		registered bool
	}
	reportedBy := map[string]int{}
	var cands []candidate
	for _, m := range machines {
		if m.Name == target.Name || !m.DeletionTimestamp.IsZero() {
			continue
		}
		seen := map[string]bool{}
		for _, a := range m.Status.Addresses {
			ip := net.ParseIP(a.Address)
			if ip == nil || !ip.IsGlobalUnicast() {
				continue
			}
			key := ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			reportedBy[key]++
			if !ours[key] || skip[key] {
				continue
			}
			cands = append(cands, candidate{ip: ip, registered: m.Status.NodeRef.IsDefined()})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].registered != cands[j].registered {
			return cands[i].registered
		}
		return bytes.Compare(cands[i].ip.To16(), cands[j].ip.To16()) < 0
	})
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		// An address two Machines report does not name one member.
		if key := c.ip.String(); reportedBy[key] == 1 {
			out = append(out, key)
		}
	}
	return out
}

// leaveExcludedHosts are the hosts the etcd-leave traffic never goes to
// directly: the control-plane endpoint and the kube-vip address.
func leaveExcludedHosts(kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) []string {
	hosts := []string{cluster.Spec.ControlPlaneEndpoint.Host}
	if kcp.Spec.HA != nil && kcp.Spec.HA.VIP != nil {
		hosts = append(hosts, kcp.Spec.HA.VIP.Address)
	}
	return hosts
}

// leaveConn is the workload-cluster connection one etcd-leave reconcile uses.
type leaveConn struct {
	client client.Client
	// via names the apiserver that answered, for logs: a member IP, or
	// leaveViaEndpoint.
	via string
	// cm is the leave ConfigMap as read through client. It is empty, never
	// nil, when the ConfigMap does not exist.
	cm *corev1.ConfigMap
}

// connectForLeave finds an apiserver for the etcd-leave traffic of target and
// reads the leave ConfigMap through it. It tries the leaveCandidateIPs in
// order and keeps the first member whose read succeeds; a read that finds no
// ConfigMap counts as an answer. When there is no candidate, or none
// answers, it tries the endpoint client. It returns an error only when
// nothing answered, and that error names every apiserver it tried.
func (r *KairosControlPlaneReconciler) connectForLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, target *clusterv1.Machine) (leaveConn, error) {
	factory := r.MemberClientFactory
	if factory == nil {
		factory = r.defaultMemberClient
	}
	var errs []error
	for _, ip := range leaveCandidateIPs(machines, target, leaveExcludedHosts(kcp, cluster)...) {
		conn, err := readLeaveThrough(ctx, ip, func() (client.Client, error) { return factory(ctx, cluster, ip) })
		if err == nil {
			return conn, nil
		}
		log.V(1).Info("etcd-leave: member did not answer; trying the next", "machine", target.Name, "member", ip, "reason", err.Error())
		errs = append(errs, fmt.Errorf("member %s: %w", ip, err))
	}
	conn, err := readLeaveThrough(ctx, leaveViaEndpoint, func() (client.Client, error) { return r.workloadClientFor(ctx, cluster) })
	if err == nil {
		return conn, nil
	}
	errs = append(errs, fmt.Errorf("%s: %w", leaveViaEndpoint, err))
	return leaveConn{}, errors.Join(errs...)
}

// readLeaveThrough builds a client with build and reads the leave ConfigMap
// through it.
func readLeaveThrough(ctx context.Context, via string, build func() (client.Client, error)) (leaveConn, error) {
	wc, err := build()
	if err != nil {
		return leaveConn{}, fmt.Errorf("build client: %w", err)
	}
	cm := &corev1.ConfigMap{}
	if err := wc.Get(ctx, leaveConfigMapKey, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return leaveConn{}, fmt.Errorf("read ConfigMap %s: %w", leaveConfigMapKey, err)
		}
		cm = &corev1.ConfigMap{}
	}
	return leaveConn{client: wc, via: via, cm: cm}, nil
}
