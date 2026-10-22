package kubevirtenv

import (
	"strings"
	"testing"
)

// The fixtures below are lifted from the guest serial log of the run this
// detector was written for, kairos-io/cluster-api-provider-kairos actions run
// 36064655730 (2026-09-24, the e2e that spent 50 minutes reporting
// WaitingForNodePush while the guest sat in the live installer's shell). The
// Actions timestamp prefix is stripped, because kubectl exec reads the raw file.

const (
	liveMediaBanner = `BdsDxe: failed to load Boot0001 "UEFI Misc Device" from PciRoot(0x0)/Pci(0x2,0x6)/Pci(0x0,0x0): Not Found
BdsDxe: loading Boot0002 "UEFI Misc Device 2" from PciRoot(0x0)/Pci(0x2,0x7)/Pci(0x0,0x0)
  Booting ` + "`Kairos'" + `
Loading kernel...
Loading initrd...
[    0.000000] Linux version 7.1.0-hadron (root@buildkitsandbox) (gcc (GCC) 15.2.0) #1 SMP PREEMPT_DYNAMIC Tue Jun 16 09:21:07 UTC 2026
[    0.000000] Command line: BOOT_IMAGE=(cd0)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable vga=795 nomodeset install-mode selinux=0 rd.live.overlay.overlayfs
[    0.000000] x86/CPU: Model not found in latest microcode list
[   11.166603] systemd[1]: Starting Load udev Rules from Credentials...
[   13.150715] Error: Driver 'pcspkr' is already registered, aborting...
`

	liveMediaShell = `
kairos-4b6b login: root (automatic login)

Refer to https://kairos.io for documentation.
`

	// stalledSerialLog is the whole failure: live media booted, shell reached,
	// nothing else for the remaining 50 minutes.
	stalledSerialLog = liveMediaBanner + liveMediaShell

	// installedSystemBanner is the second boot a healthy run produces after the
	// unattended install writes /dev/vda and reboots.
	installedSystemBanner = `
[    0.000000] Linux version 7.1.0-hadron (root@buildkitsandbox) (gcc (GCC) 15.2.0) #1 SMP PREEMPT_DYNAMIC Tue Jun 16 09:21:07 UTC 2026
[    0.000000] Command line: BOOT_IMAGE=/boot/vmlinuz root=LABEL=COS_STATE console=tty1 console=ttyS0
`
)

func TestDetectLiveMediaStallReportsTheStalledRun(t *testing.T) {
	got := DetectLiveMediaStall(stalledSerialLog)
	if got == "" {
		t.Fatal("want a stall reason for the live-media shell serial log, got none: this is the exact capture from run 36064655730, and failing to recognise it puts the e2e back on the 50-minute timeout")
	}
	// The whole point of failing fast is that the message names the real cause
	// instead of WaitingForNodePush, so assert it carries the evidence.
	for _, want := range []string{"cdroot", "install", "WaitingForNodePush"} {
		if !strings.Contains(got, want) {
			t.Errorf("stall reason does not mention %q, which is what makes it more useful than the timeout: %s", want, got)
		}
	}
}

func TestDetectLiveMediaStallIgnoresHealthyAndAmbiguousLogs(t *testing.T) {
	for name, log := range map[string]string{
		// A guest that installed and rebooted has two kernel banners. This is
		// what every passing run looks like by the time it is done.
		"installed and rebooted": stalledSerialLog + installedSystemBanner,

		// The install is running. It may still take another 25 minutes on a
		// slow runner, and failing it here would break every healthy e2e.
		"install in progress": liveMediaBanner + liveMediaShell +
			"\n[   41.882311] kairos-agent: Installing Kairos onto /dev/vda\n",
		"install in progress, immucore only": liveMediaBanner + liveMediaShell +
			"\n[   38.100000] immucore: mounting /run/initramfs/cos-state\n",
		"install in progress, yip stage only": liveMediaBanner + liveMediaShell +
			"\n[   35.000000] Running stage: kairos-install.pre\n",

		// Still booting: the shell has not been reached, so there is nothing to
		// conclude yet and the detector must stay quiet.
		"live media still booting": liveMediaBanner,

		// Not the live media at all.
		"installed system only": strings.Replace(installedSystemBanner, "\n\n", "\n", 1) + liveMediaShell,

		// A 2 MiB tail that no longer carries a kernel banner. Counting boots
		// rather than assuming one keeps this from being read as a stall.
		"tail truncated past the kernel banner": liveMediaShell,

		"empty": "",
	} {
		if got := DetectLiveMediaStall(log); got != "" {
			t.Errorf("%s: want no stall reason, got %q", name, got)
		}
	}
}

// The detector's four conditions are an AND, and a redundant one would be a
// silent liability: it would look like it protected a healthy run while
// actually contributing nothing. Drop each condition's evidence from the real
// stalled log in turn and check that the verdict flips, which proves every
// condition is load-bearing.
func TestEachConditionOfTheStallPredicateIsLoadBearing(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"second boot": func(s string) string { return s + installedSystemBanner },
		"not live media": func(s string) string {
			return strings.Replace(s, "cdroot ", "", 1)
		},
		"shell not reached": func(s string) string {
			return strings.Replace(
				strings.Replace(s, "login: root (automatic login)", "", 1),
				"Refer to https://kairos.io for documentation.", "", 1)
		},
		"install started": func(s string) string { return s + "\nkairos-agent: starting install\n" },
	} {
		if DetectLiveMediaStall(stalledSerialLog) == "" {
			t.Fatal("precondition: the unmutated log must be reported as a stall")
		}
		if got := DetectLiveMediaStall(mutate(stalledSerialLog)); got != "" {
			t.Errorf("%s: mutating the log so this condition no longer holds should suppress the stall verdict, but got %q. That condition is not load-bearing", name, got)
		}
	}
}

func TestKernelCommandLineReadsTheFirstBootsCmdline(t *testing.T) {
	cmdline, ok := kernelCommandLine(stalledSerialLog + installedSystemBanner)
	if !ok {
		t.Fatal("want the kernel command line to be found")
	}
	if !strings.HasPrefix(cmdline, "BOOT_IMAGE=(cd0)/boot/kernel cdroot") {
		t.Errorf("want the FIRST boot's command line (the live media one), got %q", cmdline)
	}
	if strings.HasSuffix(cmdline, " ") || strings.HasPrefix(cmdline, " ") {
		t.Errorf("command line should be trimmed, got %q", cmdline)
	}

	if _, ok := kernelCommandLine("no kernel banner here"); ok {
		t.Error("want ok=false when the log carries no Command line: entry")
	}
}
