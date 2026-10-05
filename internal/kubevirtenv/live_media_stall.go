package kubevirtenv

import (
	"fmt"
	"strings"
)

// The workload VM boots the Kairos live installer ISO, which is expected to run
// an unattended install (the baked `install.auto/device/reboot` triple, see
// createCloudConfigSecret) and reboot into the installed system. When that does
// not happen the live media instead drops to an interactive root shell and sits
// there, and every downstream symptom is misleading:
//
//   - the KubevirtMachine reports Ready=True, BootstrapExecSucceeded=True and
//     VMProvisioned=True, because the infrastructure side really is healthy
//   - the KairosControlPlane burns the full control-plane timeout and then
//     reports `reason: WaitingForNodePush`, which points the reader at node
//     networking rather than at an install that never started
//
// The guest serial log is the only place the real cause is visible, so these
// helpers read it directly instead of waiting for the timeout. See
// kairos-io/kairos#4991 for the run this was written from.

// liveMediaBootMarker appears once per kernel boot. A guest that installed and
// rebooted has at least two; a guest still sitting on the live media has one.
const liveMediaBootMarker = "] Linux version "

// liveMediaCmdlineMarker identifies a boot from the live ISO. `cdroot` is also
// what guards kairos-k3s-post-bootstrap.service
// (ConditionKernelCommandLine=!cdroot), so its presence on the only boot in the
// log is exactly why the bootstrap unit never fired.
const liveMediaCmdlineMarker = "cdroot"

// liveMediaShellMarkers are printed when the live media reaches its autologin
// shell. Either one means the guest finished booting the installer media.
var liveMediaShellMarkers = []string{
	"login: root (automatic login)",
	"Refer to https://kairos.io for documentation.",
}

// installActivityMarkers are strings the guest prints once the install has
// started. Any of them means the installer is doing its job, however slowly,
// and the guest is not stalled. Keep this list generous: a false entry here
// only costs a missed fast-fail, while a missing one would fail a healthy run.
var installActivityMarkers = []string{
	"kairos-agent",
	"immucore",
	"elemental",
	"cos-setup",
	"Running stage",
	"yip",
}

// DetectLiveMediaStall reports whether a guest serial log shows a VM that
// booted the Kairos live installer media, reached its interactive shell, and
// never started the unattended install. It returns an empty string when the log
// shows anything else, including an install in progress and a guest that has
// already rebooted into the installed system.
//
// The predicate is deliberately conservative: every one of the four conditions
// has to hold, so an install that is merely slow, a log that was truncated past
// its kernel banner, or a guest that rebooted cannot trip it.
func DetectLiveMediaStall(serialLog string) string {
	if serialLog == "" {
		return ""
	}

	// A guest that installed and rebooted has booted more than once. Requiring
	// exactly one boot also makes a truncated tail safe: if the tail no longer
	// carries a kernel banner the count is zero and nothing fires.
	if boots := strings.Count(serialLog, liveMediaBootMarker); boots != 1 {
		return ""
	}

	cmdline, ok := kernelCommandLine(serialLog)
	if !ok || !strings.Contains(cmdline, liveMediaCmdlineMarker) {
		return ""
	}

	if !containsAny(serialLog, liveMediaShellMarkers) {
		return ""
	}

	if containsAny(serialLog, installActivityMarkers) {
		return ""
	}

	return fmt.Sprintf(
		"guest booted the live installer media and stopped at its interactive shell: "+
			"the serial log has exactly one boot, its kernel command line is the live "+
			"media's (%q), and it contains none of %v, so the unattended install never "+
			"started. /dev/vda was never written, the guest never rebooted, and "+
			"kairos-k3s-post-bootstrap.service was skipped by its own "+
			"ConditionKernelCommandLine=!cdroot guard, which is why the control plane "+
			"reports WaitingForNodePush. Check that the OSArtifact baked the install: "+
			"block into the ISO (see createCloudConfigSecret)",
		cmdline, installActivityMarkers)
}

// kernelCommandLine returns the first kernel command line the log records. The
// serial log carries terminal escape sequences and, on the Kairos live media,
// a `Command line:` line per boot.
func kernelCommandLine(serialLog string) (string, bool) {
	const marker = "] Command line: "
	for _, line := range strings.Split(serialLog, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):]), true
		}
	}
	return "", false
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
