/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY KIND, either express or implied.
See the License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
)

// PreparedKindCluster describes a kind cluster ready for kubectl and kustomize apply dev.
type PreparedKindCluster struct {
	ClusterName string
	Kubeconfig  string
	ImageRef    string
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

// RepoRoot returns the repository root (directory containing go.mod) starting from the process working directory.
func RepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	return findRepoRoot(wd)
}

func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %q", start)
		}
		dir = parent
	}
}
