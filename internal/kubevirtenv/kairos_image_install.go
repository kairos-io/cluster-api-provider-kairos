package kubevirtenv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	kairosCloudImageName = "kairos-kubevirt"
	cdiUploadDefaultPort = 18443
	// kairosOSArtifactDiskMiB is spec.artifacts.diskSize (MiB); the built .raw virtual size matches this.
	kairosOSArtifactDiskMiB = 32000
	// kairosCDIUploadExtraMiB is added to the CDI DataVolume virtctl --size so the PVC exceeds the raw image
	// (provisioner rounding and upload checks compare against available capacity; 25Gi was too small for 32000MiB disk).
	kairosCDIUploadExtraMiB = 2048
)

func (e *Environment) kairosImageBuildDir() string {
	return filepath.Join(e.WorkDir, "kairos-image", "build")
}

// kairosOSArtifactBaseImage is a published Kairos core image matching the host GOARCH (operator Stage 1 ref).
func kairosOSArtifactBaseImage(goarch string) string {
	switch goarch {
	case "arm64":
		return "quay.io/kairos/opensuse:leap-15.6-core-arm64-generic-v3.6.0"
	default:
		return "quay.io/kairos/opensuse:leap-15.6-core-amd64-generic-v3.6.0"
	}
}

func (e *Environment) resolveVirtctl() (string, error) {
	p := e.virtctlBin()
	if filepath.IsAbs(p) || strings.Contains(p, string(os.PathSeparator)) {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if path, err := exec.LookPath("virtctl"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("virtctl not found (expected pinned binary at %q after EnsurePinnedCLIs)", e.VirtctlPath)
}

// BuildKairosCloudImage creates OSArtifact and downloads the built raw image into the workdir.
func (e *Environment) BuildKairosCloudImage(ctx context.Context) error {
	log := e.log()
	log.Step("Building Kairos cloud image using OSArtifact CR...")
	if err := e.RequireKubeconfig(); err != nil {
		return err
	}
	if err := os.MkdirAll(e.WorkDir, 0o755); err != nil {
		return err
	}
	buildDir := e.kairosImageBuildDir()
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	cfg, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	if err := e.createCloudConfigSecret(ctx, clientset); err != nil {
		return fmt.Errorf("cloud-config secret: %w", err)
	}
	if err := e.createOSArtifactCR(ctx, dynamicClient); err != nil {
		return fmt.Errorf("OSArtifact: %w", err)
	}
	if err := e.waitForOSArtifactReady(ctx, dynamicClient); err != nil {
		return fmt.Errorf("wait OSArtifact: %w", err)
	}
	if err := e.downloadImageFromNginx(ctx, clientset, buildDir); err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	log.Step("Kairos image build complete ✓")
	return nil
}

// UploadKairosDataVolume uploads the built raw image to CDI via virtctl.
func (e *Environment) UploadKairosDataVolume(ctx context.Context) error {
	log := e.log()
	log.Step("Uploading Kairos image using virtctl...")
	if err := e.RequireKubeconfig(); err != nil {
		return err
	}
	imageFile, err := e.findKairosImageFile()
	if err != nil {
		return err
	}
	log.Infof("Using image file: %s", imageFile)
	virtctlPath, err := e.resolveVirtctl()
	if err != nil {
		return err
	}
	log.Infof("Using virtctl: %s", virtctlPath)

	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	if _, err := clientset.CoreV1().Services("cdi").Get(ctx, "cdi-uploadproxy", metav1.GetOptions{}); err != nil {
		return fmt.Errorf("CDI upload proxy not found (install CDI first): %w", err)
	}

	cfg, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	dvGVR := schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
	if _, err := dynamicClient.Resource(dvGVR).Namespace("default").Get(ctx, kairosCloudImageName, metav1.GetOptions{}); err == nil {
		log.Infof("DataVolume %s exists; deleting for fresh upload...", kairosCloudImageName)
		_ = dynamicClient.Resource(dvGVR).Namespace("default").Delete(ctx, kairosCloudImageName, metav1.DeleteOptions{})
		time.Sleep(2 * time.Second)
	}

	port := cdiUploadDefaultPort
	if envPort := os.Getenv("CDI_UPLOAD_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			port = p
		}
	}
	log.Infof("Setting up port-forward on port %d...", port)
	kcfg := e.KubeconfigPath()
	kctx := e.KubectlContext()
	pf := exec.CommandContext(ctx, e.kubectlBin(), "port-forward", "-n", "cdi", "service/cdi-uploadproxy",
		fmt.Sprintf("%d:443", port), "--kubeconfig", kcfg, "--context", kctx)
	stdout, stderr := e.execOut()
	pf.Stdout = stdout
	pf.Stderr = stderr
	if err := pf.Start(); err != nil {
		return fmt.Errorf("port-forward: %w", err)
	}
	defer func() {
		if pf.Process != nil {
			_ = pf.Process.Kill()
			_, _ = pf.Process.Wait()
		}
	}()
	time.Sleep(2 * time.Second)

	uploadPVCSize := fmt.Sprintf("%dMi", kairosOSArtifactDiskMiB+kairosCDIUploadExtraMiB)
	uploadURL := fmt.Sprintf("https://localhost:%d", port)
	virtctlCmd := exec.CommandContext(ctx, virtctlPath, "image-upload",
		"dv", kairosCloudImageName,
		"--size="+uploadPVCSize,
		"--access-mode=ReadWriteOnce",
		"--image-path", imageFile,
		"--uploadproxy-url", uploadURL,
		"--insecure",
		"--force-bind",
		"--wait-secs=300",
		"--kubeconfig", kcfg,
		"--context", kctx,
	)
	virtctlCmd.Stdout = stdout
	virtctlCmd.Stderr = stderr
	if err := virtctlCmd.Run(); err != nil {
		return fmt.Errorf("virtctl image-upload: %w", err)
	}
	log.Step("Image upload completed ✓")
	return nil
}

func (e *Environment) findKairosImageFile() (string, error) {
	if envFile := os.Getenv("KAIROS_IMAGE_FILE"); envFile != "" {
		if _, err := os.Stat(envFile); err == nil {
			return envFile, nil
		}
	}
	defaultFile := filepath.Join(e.kairosImageBuildDir(), fmt.Sprintf("%s.raw", kairosCloudImageName))
	if _, err := os.Stat(defaultFile); err == nil {
		return defaultFile, nil
	}
	buildDir := e.kairosImageBuildDir()
	if entries, err := os.ReadDir(buildDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, kairosCloudImageName) {
				ext := filepath.Ext(name)
				if ext == ".raw" || ext == ".qcow2" {
					return filepath.Join(buildDir, name), nil
				}
			}
		}
	}
	return "", fmt.Errorf("image file not found under %s", buildDir)
}

func (e *Environment) createCloudConfigSecret(ctx context.Context, clientset kubernetes.Interface) error {
	log := e.log()
	log.Infof("Creating cloud-config Secret...")
	cloudConfig := `#cloud-config
install:
  grub_options:
    extra_cmdline: "console=ttyS0 console=tty0"
`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-cloud-config", kairosCloudImageName),
			Namespace: "default",
		},
		Data: map[string][]byte{"userdata": []byte(cloudConfig)},
	}
	_, err := clientset.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, err := clientset.CoreV1().Secrets("default").Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	existing.Data = secret.Data
	_, err = clientset.CoreV1().Secrets("default").Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

var osArtifactGVR = schema.GroupVersionResource{Group: "build.kairos.io", Version: "v1alpha2", Resource: "osartifacts"}

// deleteOSArtifactIfPresent removes an existing OSArtifact so Apply can create a new one.
// The operator validates spec as immutable after creation; server-side apply would fail on re-runs.
func (e *Environment) deleteOSArtifactIfPresent(ctx context.Context, dynamicClient dynamic.Interface) error {
	log := e.log()
	ns := "default"
	_, err := dynamicClient.Resource(osArtifactGVR).Namespace(ns).Get(ctx, kairosCloudImageName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	log.Infof("Deleting existing OSArtifact %q (spec is immutable; recreating)", kairosCloudImageName)
	policy := metav1.DeletePropagationForeground
	if err := dynamicClient.Resource(osArtifactGVR).Namespace(ns).Delete(ctx, kairosCloudImageName, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
		return fmt.Errorf("delete OSArtifact: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		_, getErr := dynamicClient.Resource(osArtifactGVR).Namespace(ns).Get(ctx, kairosCloudImageName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("wait for OSArtifact deletion: %w", err)
	}
	return nil
}

func (e *Environment) createOSArtifactCR(ctx context.Context, dynamicClient dynamic.Interface) error {
	log := e.log()
	log.Infof("Creating OSArtifact CustomResource...")
	if err := e.deleteOSArtifactIfPresent(ctx, dynamicClient); err != nil {
		return err
	}
	goarch := runtime.GOARCH
	if goarch != "amd64" && goarch != "arm64" {
		return fmt.Errorf("OSArtifact: unsupported GOARCH %q (need amd64 or arm64)", goarch)
	}
	baseImg := kairosOSArtifactBaseImage(goarch)
	yaml := fmt.Sprintf(`apiVersion: build.kairos.io/v1alpha2
kind: OSArtifact
metadata:
  name: %s
  namespace: default
spec:
  image:
    ref: %q
  artifacts:
    arch: %s
    cloudImage: true
    diskSize: "%d"
    cloudConfigRef:
      name: %s-cloud-config
      key: userdata
  exporters:
  - template:
      spec:
        restartPolicy: Never
        containers:
        - name: upload-to-nginx
          image: curlimages/curl:latest
          command: ["sh", "-ec"]
          args:
            - |
              for f in /artifacts/*; do
                [ -f "$f" ] || continue
                base=$(basename "$f")
                curl -fsSL -T "$f" "http://kairos-operator-nginx/${base}" || exit 1
              done
          volumeMounts:
          - name: artifacts
            mountPath: /artifacts
            readOnly: true
`, kairosCloudImageName, baseImg, goarch, kairosOSArtifactDiskMiB, kairosCloudImageName)
	rc, err := e.RESTConfig()
	if err != nil {
		return err
	}
	return e.ApplyManifestContent(dynamicClient, rc, []byte(yaml))
}

func (e *Environment) waitForOSArtifactReady(ctx context.Context, dynamicClient dynamic.Interface) error {
	log := e.log()
	log.Infof("Waiting for OSArtifact to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 1800*time.Second)
	defer cancel()
	return wait.PollUntilContextCancel(waitCtx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		obj, err := dynamicClient.Resource(osArtifactGVR).Namespace("default").Get(ctx, kairosCloudImageName, metav1.GetOptions{})
		if err != nil {
			log.WriteString(".")
			return false, nil
		}
		phase, found, err := unstructured.NestedString(obj.Object, "status", "phase")
		if !found || err != nil {
			log.WriteString(".")
			return false, nil
		}
		if phase == "Ready" {
			log.Infof("✓ OSArtifact ready (phase: %s)", phase)
			return true, nil
		}
		if phase == "Error" {
			log.Warnf("OSArtifact build failed; object dump:")
			if b, err := obj.MarshalJSON(); err == nil {
				log.WriteString(string(b) + "\n")
			}
			return false, fmt.Errorf("OSArtifact build failed (phase: %s)", phase)
		}
		log.WriteString(".")
		return false, nil
	})
}

func (e *Environment) downloadImageFromNginx(ctx context.Context, clientset kubernetes.Interface, buildDir string) error {
	log := e.log()
	log.Infof("Downloading built image from nginx...")
	services, err := clientset.CoreV1().Services("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	var nginxSvc *corev1.Service
	for i := range services.Items {
		svc := &services.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		if svc.Name == "kairos-operator-nginx" {
			nginxSvc = svc
			break
		}
	}
	if nginxSvc == nil {
		for i := range services.Items {
			svc := &services.Items[i]
			if svc.Spec.Type == corev1.ServiceTypeNodePort && strings.Contains(strings.ToLower(svc.Name), "nginx") {
				nginxSvc = svc
				break
			}
		}
	}
	if nginxSvc == nil {
		return fmt.Errorf("could not find nginx service")
	}
	if len(nginxSvc.Spec.Ports) == 0 {
		return fmt.Errorf("nginx service has no ports")
	}
	nodePort := nginxSvc.Spec.Ports[0].NodePort
	if nodePort == 0 {
		return fmt.Errorf("nginx nodePort not set")
	}
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return fmt.Errorf("nodes: %w", err)
	}
	var nodeIP string
	for _, addr := range nodes.Items[0].Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}
	if nodeIP == "" {
		return fmt.Errorf("node internal IP not found")
	}
	fn := fmt.Sprintf("%s.raw", kairosCloudImageName)
	url := fmt.Sprintf("http://%s:%d/%s", nodeIP, nodePort, fn)
	outPath := filepath.Join(buildDir, fn)
	log.Infof("Downloading %s from %s", fn, url)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	log.Infof("Downloaded to: %s", outPath)
	log.Infof("Checking for built image...")
	matches, err := filepath.Glob(filepath.Join(buildDir, fmt.Sprintf("%s*", kairosCloudImageName)))
	if err == nil && len(matches) > 0 {
		for _, match := range matches {
			if info, err := os.Stat(match); err == nil && !info.IsDir() {
				log.Infof("Found: %s", match)
			}
		}
	} else {
		log.Infof("No image files found.")
	}
	return nil
}
