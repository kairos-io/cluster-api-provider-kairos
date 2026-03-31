package kubevirtenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultDockerExe = "docker"

func (e *Environment) dockerExe() string {
	if strings.TrimSpace(e.DockerExe) != "" {
		return e.DockerExe
	}
	return defaultDockerExe
}

// DockerBuildController runs docker build for the repo Dockerfile (repoRoot must be set).
func (e *Environment) DockerBuildController(ctx context.Context, imageRef string) error {
	if e.RepoRoot == "" {
		return fmt.Errorf("Environment.RepoRoot is required for DockerBuildController")
	}
	df := filepath.Join(e.RepoRoot, "Dockerfile")
	if _, err := os.Stat(df); err != nil {
		return fmt.Errorf("dockerfile: %w", err)
	}
	args := []string{"build", "-t", imageRef, "-f", df, e.RepoRoot}
	cmd := exec.CommandContext(ctx, e.dockerExe(), args...)
	cmd.Dir = e.RepoRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", e.dockerExe(), strings.Join(args, " "), err, out.String())
	}
	return nil
}

// KindLoadDockerImage loads a docker image into the kind cluster.
func (e *Environment) KindLoadDockerImage(ctx context.Context, imageRef string) error {
	args := []string{"load", "docker-image", imageRef, "--name", e.ClusterName}
	cmd := exec.CommandContext(ctx, e.kindBin(), args...)
	var out bytes.Buffer
	stdout, stderr := e.execOut()
	cmd.Stdout = ioMultiWriter(stdout, &out)
	cmd.Stderr = ioMultiWriter(stderr, &out)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

func ioMultiWriter(w io.Writer, extra *bytes.Buffer) io.Writer {
	if w == nil {
		return extra
	}
	return io.MultiWriter(w, extra)
}

// MakeDockerBuildKairosCAPI runs `make docker-build IMG=img` from RepoRoot (includes generate, fmt, vet).
func (e *Environment) MakeDockerBuildKairosCAPI(ctx context.Context, img string) error {
	if e.RepoRoot == "" {
		return fmt.Errorf("Environment.RepoRoot is required for MakeDockerBuildKairosCAPI")
	}
	cmd := exec.CommandContext(ctx, "make", "-f", "Makefile", "docker-build", "IMG="+img)
	cmd.Dir = e.RepoRoot
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make docker-build IMG=%s: %w", img, err)
	}
	return nil
}
