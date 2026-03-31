package kubevirtenv

import (
	"context"
	"fmt"
	"os"
)

// InstallBaseStackThroughCAPK creates the kind cluster (if needed) and installs Calico, local-path, CDI,
// KubeVirt, Cluster API (kubeadm core/bootstrap/control-plane), and CAPK. It does not install cert-manager,
// the Kairos operator, or the Kairos CAPI provider.
//
// Calico runs before local-path: the kind config disables the default CNI, so nodes stay NotReady until
// a CNI is applied; local-path provisioner pods need pod networking.
func InstallBaseStackThroughCAPK(ctx context.Context, env *Environment) error {
	if err := env.CreateTestCluster(ctx); err != nil {
		return fmt.Errorf("create test cluster: %w", err)
	}
	if err := env.InstallCalico(ctx); err != nil {
		return fmt.Errorf("calico: %w", err)
	}
	if err := env.InstallLocalPath(ctx); err != nil {
		return fmt.Errorf("local-path: %w", err)
	}
	if err := env.InstallCDI(ctx); err != nil {
		return fmt.Errorf("cdi: %w", err)
	}
	if err := env.InstallKubeVirt(ctx); err != nil {
		return fmt.Errorf("kubevirt: %w", err)
	}
	if err := env.InstallCAPIKubeadmCore(ctx); err != nil {
		return fmt.Errorf("capi: %w", err)
	}
	if err := env.InstallCAPK(ctx); err != nil {
		return fmt.Errorf("capk: %w", err)
	}
	return nil
}

// RunFullDemoSetup ensures pinned CLIs, provisions the kind cluster, installs the full stack through CAPK,
// then the Kairos operator (+ nginx for artifacts), Kairos cloud image build/upload, cert-manager, and the release Kairos CAPI provider.
func RunFullDemoSetup(ctx context.Context, env *Environment) error {
	log := env.log()
	if env.RepoRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("repo root: %w", err)
		}
		r, err := FindRepoRoot(wd)
		if err != nil {
			return fmt.Errorf("repo root: %w", err)
		}
		env.RepoRoot = r
	}
	log.Step("=== Full demo setup ===")
	log.Infof("Cluster name: %s", env.ClusterName)
	if err := env.EnsurePinnedCLIs(ctx); err != nil {
		return fmt.Errorf("CLIs: %w", err)
	}
	if err := InstallBaseStackThroughCAPK(ctx, env); err != nil {
		return err
	}
	log.Step("Installing Kairos operator (OSArtifact builds)...")
	if err := env.InstallKairosOperator(ctx); err != nil {
		return fmt.Errorf("kairos operator: %w", err)
	}
	log.Step("Building Kairos cloud image...")
	if err := env.BuildKairosCloudImage(ctx); err != nil {
		return fmt.Errorf("build Kairos image: %w", err)
	}
	log.Step("Uploading Kairos image to CDI...")
	if err := env.UploadKairosDataVolume(ctx); err != nil {
		return fmt.Errorf("upload Kairos image: %w", err)
	}
	log.Step("Installing cert-manager...")
	if err := env.InstallCertManager(ctx); err != nil {
		return fmt.Errorf("cert-manager: %w", err)
	}
	log.Step("Installing Kairos CAPI provider (release image)...")
	if err := env.InstallKairosCAPIProviderRelease(ctx); err != nil {
		return fmt.Errorf("kairos provider: %w", err)
	}
	log.Step("=== Setup complete ===")
	return nil
}

// RunDevManagementStack matches the e2e path: pinned CLIs, kind + base stack through CAPK, cert-manager,
// docker build of the dev controller image, kind load, and kubectl apply of config/dev.
func RunDevManagementStack(ctx context.Context, env *Environment) error {
	log := env.log()
	if env.RepoRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("repo root: %w", err)
		}
		r, err := FindRepoRoot(wd)
		if err != nil {
			return fmt.Errorf("repo root: %w", err)
		}
		env.RepoRoot = r
	}
	log.Step("=== Dev management stack (e2e) ===")
	log.Infof("Cluster name: %s", env.ClusterName)
	if err := env.EnsurePinnedCLIs(ctx); err != nil {
		return fmt.Errorf("CLIs: %w", err)
	}
	if err := InstallBaseStackThroughCAPK(ctx, env); err != nil {
		return err
	}
	log.Step("Installing cert-manager...")
	if err := env.InstallCertManager(ctx); err != nil {
		return fmt.Errorf("cert-manager: %w", err)
	}
	log.Step(fmt.Sprintf("Docker build + kind load (%q)", DevControllerImageRef))
	if err := env.DockerBuildController(ctx, DevControllerImageRef); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	if err := env.KindLoadDockerImage(ctx, DevControllerImageRef); err != nil {
		return fmt.Errorf("kind load: %w", err)
	}
	log.Step("Applying config/dev (kustomize | kubectl apply -f -)...")
	if err := env.ApplyDevKustomize(ctx); err != nil {
		return fmt.Errorf("apply dev: %w", err)
	}
	log.Step("=== Dev stack ready ===")
	return nil
}
