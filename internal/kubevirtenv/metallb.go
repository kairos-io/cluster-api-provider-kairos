package kubevirtenv

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

const (
	MetalLBVersion     = "v0.14.9"
	MetalLBManifestURL = "https://raw.githubusercontent.com/metallb/metallb/%s/config/manifests/metallb-native.yaml"
	MetalLBNamespace   = "metallb-system"
)

// IsMetalLBInstalled reports whether the MetalLB controller deployment is available.
func (e *Environment) IsMetalLBInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deployment, err := clientset.AppsV1().Deployments(MetalLBNamespace).Get(ctx, "controller", metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// InstallMetalLB installs MetalLB and configures an L2 IP address pool derived from the kind Docker network.
func (e *Environment) InstallMetalLB(ctx context.Context) error {
	log := e.log()
	if e.IsMetalLBInstalled() {
		log.Step("MetalLB is already installed ✓")
		return nil
	}

	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	manifestURL := fmt.Sprintf(MetalLBManifestURL, MetalLBVersion)
	log.Infof("Installing MetalLB %s...", MetalLBVersion)
	if err := e.ApplyManifestFromURL(ctx, dynamicClient, config, manifestURL); err != nil {
		return fmt.Errorf("apply MetalLB manifest: %w", err)
	}

	log.Step("Waiting for MetalLB controller...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	if err := WaitForDeployment(waitCtx, clientset, MetalLBNamespace, "controller"); err != nil {
		return fmt.Errorf("MetalLB controller: %w", err)
	}

	log.Step("Waiting for MetalLB speaker...")
	if err := WaitForDaemonset(waitCtx, log, clientset, MetalLBNamespace, "speaker"); err != nil {
		return fmt.Errorf("MetalLB speaker: %w", err)
	}

	ipRange, err := e.kindDockerSubnetRange()
	if err != nil {
		return fmt.Errorf("determine MetalLB IP range: %w", err)
	}
	log.Infof("Configuring MetalLB L2 address pool: %s", ipRange)

	poolYAML := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-pool
  namespace: %s
spec:
  addresses:
  - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kind-l2
  namespace: %s
spec:
  ipAddressPools:
  - kind-pool
`, MetalLBNamespace, ipRange, MetalLBNamespace)

	if err := e.ApplyManifestContent(ctx, dynamicClient, config, []byte(poolYAML)); err != nil {
		return fmt.Errorf("apply MetalLB pool config: %w", err)
	}

	log.Step("MetalLB installed ✓")
	return nil
}

// kindDockerSubnetRange inspects the kind Docker network and returns an IP range
// (e.g. "172.18.255.200-172.18.255.250") suitable for MetalLB.
func (e *Environment) kindDockerSubnetRange() (string, error) {
	dockerExe := e.DockerExe
	if dockerExe == "" {
		dockerExe = "docker"
	}

	out, err := exec.Command(dockerExe, "network", "inspect", "kind", "-f", "{{json .IPAM.Config}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker network inspect kind: %w", err)
	}

	var configs []struct {
		Subnet string `json:"Subnet"`
	}
	if err := json.Unmarshal(out, &configs); err != nil {
		return "", fmt.Errorf("parse docker network config: %w (raw: %s)", err, strings.TrimSpace(string(out)))
	}

	// Find the first IPv4 subnet.
	for _, cfg := range configs {
		ip, ipNet, err := net.ParseCIDR(cfg.Subnet)
		if err != nil {
			continue
		}
		if ip.To4() == nil {
			continue // skip IPv6
		}
		return metalLBRangeFromSubnet(ipNet)
	}
	return "", fmt.Errorf("no IPv4 subnet found in kind Docker network")
}

// metalLBRangeFromSubnet picks a small range at the high end of the subnet.
// For example, 172.18.0.0/16 → "172.18.255.200-172.18.255.250".
func metalLBRangeFromSubnet(ipNet *net.IPNet) (string, error) {
	ip := ipNet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("not an IPv4 subnet: %s", ipNet)
	}
	mask := ipNet.Mask

	// Compute broadcast address (last address in the subnet).
	broadcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		broadcast[i] = ip[i] | ^mask[i]
	}

	// Use broadcast-50 to broadcast-1 as the range.
	end := make(net.IP, 4)
	copy(end, broadcast)
	end[3]-- // broadcast - 1

	start := make(net.IP, 4)
	copy(start, end)
	// Subtract 50 from the last octet, borrowing if needed.
	val := int(start[2])<<8 + int(start[3]) - 50
	if val < 0 {
		return "", fmt.Errorf("subnet %s too small for MetalLB range", ipNet)
	}
	start[2] = byte(val >> 8)
	start[3] = byte(val & 0xff)

	return fmt.Sprintf("%s-%s", start, end), nil
}
