package kubevirtenv

import (
	"strings"
	"testing"
)

// The e2e stack used to call clusterctl init twice in a row, once for CAPI and
// once for the KubeVirt infrastructure provider. clusterctl bootstraps
// cert-manager on every init and probes for an existing install with a single
// non-retried attempt, so the second init's probe raced the first one's
// teardown; losing that race cost a 10 minute reinstall wait and killed the
// job (kairos-io/kairos#5501). These tests pin the property that fixes it: one
// command carries every provider that still has to be installed.

func TestClusterctlInitAsksForEveryMissingProviderInOneCommand(t *testing.T) {
	args := clusterctlInitArgs("/tmp/kubeconfig", "v1.13.4", "kubevirt", true, true)

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--core cluster-api:v1.13.4",
		"--bootstrap kubeadm:v1.13.4",
		"--control-plane kubeadm:v1.13.4",
		"--infrastructure kubevirt",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("clusterctlInitArgs omitted %q; got %q", want, joined)
		}
	}
	if got := countInit(args); got != 1 {
		t.Errorf("args describe %d init commands, want exactly 1 (a second init re-probes cert-manager): %q", got, joined)
	}
	if args[0] != "init" {
		t.Errorf("args[0] = %q, want %q", args[0], "init")
	}
	if !strings.Contains(joined, "--kubeconfig /tmp/kubeconfig") {
		t.Errorf("args dropped the kubeconfig: %q", joined)
	}
}

func TestClusterctlInitLeavesOutWhatIsAlreadyInstalled(t *testing.T) {
	// Re-running against a cluster that already carries CAPI: only the
	// infrastructure provider is requested, and no empty version is passed.
	capkOnly := strings.Join(clusterctlInitArgs("/tmp/kubeconfig", "", "kubevirt:v0.11.2", false, true), " ")
	if strings.Contains(capkOnly, "--core") || strings.Contains(capkOnly, "--bootstrap") || strings.Contains(capkOnly, "--control-plane") {
		t.Errorf("CAPI is installed, so its flags must not be sent again: %q", capkOnly)
	}
	if !strings.Contains(capkOnly, "--infrastructure kubevirt:v0.11.2") {
		t.Errorf("args dropped the pinned infrastructure provider: %q", capkOnly)
	}

	capiOnly := strings.Join(clusterctlInitArgs("/tmp/kubeconfig", "v1.13.4", "kubevirt", true, false), " ")
	if strings.Contains(capiOnly, "--infrastructure") {
		t.Errorf("CAPK is installed, so --infrastructure must not be sent again: %q", capiOnly)
	}
	if !strings.Contains(capiOnly, "--core cluster-api:v1.13.4") {
		t.Errorf("args dropped the CAPI core provider: %q", capiOnly)
	}
}

func countInit(args []string) int {
	n := 0
	for _, a := range args {
		if a == "init" {
			n++
		}
	}
	return n
}
