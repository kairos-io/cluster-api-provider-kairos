/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY KIND, either express or implied. See the
License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DevControllerImageRef is the tag expected by config/dev after kustomize (image must be loaded into kind).
const DevControllerImageRef = "controller:latest"

// PrepareKindClusterInput configures PrepareKindManagementClusterWithLocalImage.
type PrepareKindClusterInput struct {
	// ClusterName is the kind cluster name (must be unique per run).
	ClusterName string
	// RepoRoot is the module root containing the Dockerfile.
	RepoRoot string
	// WorkDir holds the kubeconfig at WorkDir/kubeconfig.
	WorkDir string
	// ImageRef is passed to docker build -t and kind load (default: DevControllerImageRef).
	ImageRef string
	// Docker is the docker executable name or path (default: E2E_DOCKER env or "docker").
	Docker string
}

// PreparedKindCluster describes a kind cluster ready for kubectl and kustomize apply dev.
type PreparedKindCluster struct {
	ClusterName string
	Kubeconfig  string
	ImageRef    string
}

func (in *PrepareKindClusterInput) imageRef() string {
	if in.ImageRef != "" {
		return in.ImageRef
	}
	return DevControllerImageRef
}

func (in *PrepareKindClusterInput) dockerExe() string {
	return DockerExeFromEnvOrInput(in.Docker)
}

// DockerExeFromEnvOrInput returns inDocker if non-empty, else E2E_DOCKER, else "docker".
func DockerExeFromEnvOrInput(inDocker string) string {
	if inDocker != "" {
		return inDocker
	}
	if v := os.Getenv("E2E_DOCKER"); v != "" {
		return v
	}
	return "docker"
}

// PrepareKindManagementClusterWithLocalImage creates a kind cluster, builds the controller image at RepoRoot,
// and loads ImageRef into the cluster node(s). Kubeconfig is written to WorkDir/kubeconfig.
func PrepareKindManagementClusterWithLocalImage(ctx context.Context, tools Tools, in PrepareKindClusterInput) (*PreparedKindCluster, error) {
	if in.ClusterName == "" {
		return nil, fmt.Errorf("PrepareKindClusterInput: ClusterName is required")
	}
	if in.RepoRoot == "" {
		return nil, fmt.Errorf("PrepareKindClusterInput: RepoRoot is required")
	}
	if in.WorkDir == "" {
		return nil, fmt.Errorf("PrepareKindClusterInput: WorkDir is required")
	}
	if err := os.MkdirAll(in.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}
	kcfg := filepath.Join(in.WorkDir, "kubeconfig")
	img := in.imageRef()
	docker := in.dockerExe()

	if err := KindCreateCluster(ctx, tools, in.ClusterName, kcfg); err != nil {
		return nil, err
	}
	if err := DockerBuildController(ctx, docker, in.RepoRoot, img); err != nil {
		return nil, err
	}
	if err := KindLoadDockerImage(ctx, tools, in.ClusterName, img); err != nil {
		return nil, err
	}
	return &PreparedKindCluster{
		ClusterName: in.ClusterName,
		Kubeconfig:  kcfg,
		ImageRef:    img,
	}, nil
}

// KindCreateCluster runs kind create cluster and waits until the control plane is ready.
// kind v0.20+ requires --wait <duration> (Go duration string, e.g. 15m), not a boolean flag.
func KindCreateCluster(ctx context.Context, tools Tools, name, kubeconfig string) error {
	return KindCreateClusterWithConfig(ctx, tools, name, kubeconfig, "")
}

// KindCreateClusterWithConfig is like KindCreateCluster but passes --config when configPath is non-empty.
func KindCreateClusterWithConfig(ctx context.Context, tools Tools, name, kubeconfig, configPath string) error {
	wait := os.Getenv("E2E_KIND_CREATE_WAIT")
	if wait == "" {
		wait = "15m"
	}
	args := []string{"create", "cluster", "--name", name, "--kubeconfig", kubeconfig, "--wait", wait}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	cmd := tools.Kind(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

// KindLoadDockerImage runs kind load docker-image.
func KindLoadDockerImage(ctx context.Context, tools Tools, clusterName, imageRef string) error {
	args := []string{"load", "docker-image", imageRef, "--name", clusterName}
	cmd := tools.Kind(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

// DockerBuildController runs docker build for the repo Dockerfile.
func DockerBuildController(ctx context.Context, dockerExe, repoRoot, imageRef string) error {
	df := filepath.Join(repoRoot, "Dockerfile")
	if _, err := os.Stat(df); err != nil {
		return fmt.Errorf("dockerfile: %w", err)
	}
	args := []string{"build", "-t", imageRef, "-f", df, repoRoot}
	cmd := exec.CommandContext(ctx, dockerExe, args...)
	cmd.Dir = repoRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", dockerExe, strings.Join(args, " "), err, out.String())
	}
	return nil
}

// KindDeleteCluster removes a kind cluster (best-effort for teardown).
func KindDeleteCluster(ctx context.Context, tools Tools, name string) error {
	cmd := tools.Kind(ctx, "delete", "cluster", "--name", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind delete cluster --name %s: %w\n%s", name, err, out.String())
	}
	return nil
}
