package kubevirtenv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// installerCloudConfig is the cloud-config the OSArtifact bakes into the live
// installer ISO through `spec.artifacts.cloudConfigRef`. It is declared here,
// next to the check that proves it survived the build, so the Secret and the
// assertion can never drift apart.
const installerCloudConfig = `#cloud-config
install:
  auto: true
  device: "/dev/vda"
  reboot: true
  grub_options:
    extra_cmdline: "console=ttyS0 systemd.journald.forward_to_console=1"
`

// bakedCloudConfigMarkers are substrings of installerCloudConfig that appear in
// no other part of a Kairos ISO. A whole-file match is deliberately not used:
// AuroraBoot is free to re-indent or re-serialise the YAML on its way to the
// ISO root, and an assertion that fails on reformatting would be worse than no
// assertion at all. These three carry the whole meaning of the bake - the
// unattended install, the target disk, and the serial console that makes the
// installed boot observable - so their absence is decisive.
var bakedCloudConfigMarkers = []string{
	"auto: true",
	`device: "/dev/vda"`,
	"console=ttyS0 systemd.journald.forward_to_console=1",
}

// bakedScanChunk is the read size for scanning the ISO. The ISO is ~400MB, so
// it is streamed rather than read whole, with an overlap carried between reads
// so a marker straddling a chunk boundary is still found.
const bakedScanChunk = 4 << 20

// errNoBakedCloudConfig reports that the published ISO does not carry the
// cloud-config the OSArtifact was told to bake.
var errNoBakedCloudConfig = errors.New("live installer ISO does not carry the baked cloud-config")

// verifyBakedCloudConfig fails when the ISO at path does not contain the
// cloud-config that spec.artifacts.cloudConfigRef asked the operator to bake.
//
// Without it the live media boots to an interactive root shell instead of
// running the unattended install, /dev/vda is never written, and the node never
// reboots into an installed system. The control plane then sits at
// KubeconfigReady=False/WaitingForNodePush for its full 50 minute timeout and
// blames node networking, which is the wrong place to look. Checking the
// artifact turns that into an immediate and accurate failure
// (kairos-io/kairos#4991).
//
// Files at the ISO root are stored uncompressed, so the bytes AuroraBoot wrote
// are present verbatim and a byte scan is enough. No ISO9660 reader is needed.
func verifyBakedCloudConfig(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open built image %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	missing, err := scanForMarkers(f, bakedCloudConfigMarkers)
	if err != nil {
		return fmt.Errorf("scan built image %s: %w", path, err)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: %s is missing %v. The OSArtifact reported Ready, so the build "+
			"succeeded but published an image without the cloud-config from "+
			"spec.artifacts.cloudConfigRef. The live installer will drop to a "+
			"root shell instead of installing, and the control plane will time "+
			"out on WaitingForNodePush 50 minutes from now",
		errNoBakedCloudConfig, path, missing)
}

// scanForMarkers streams r and returns the markers it never saw, in the order
// they were given.
func scanForMarkers(r io.Reader, markers []string) ([]string, error) {
	found := make([]bool, len(markers))
	overlap := 0
	for _, m := range markers {
		if len(m) > overlap {
			overlap = len(m)
		}
	}
	if overlap > 0 {
		overlap--
	}

	buf := make([]byte, overlap+bakedScanChunk)
	carry := 0
	for {
		n, err := io.ReadFull(r, buf[carry:])
		window := buf[:carry+n]
		for i, m := range markers {
			if !found[i] && bytes.Contains(window, []byte(m)) {
				found[i] = true
			}
		}
		if err != nil {
			// ReadFull reports a short final read as ErrUnexpectedEOF; both it
			// and EOF mean the reader is drained, not that the scan failed.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		// Carry the tail forward so a marker split across two reads is seen.
		carry = overlap
		if len(window) < carry {
			carry = len(window)
		}
		copy(buf, window[len(window)-carry:])
	}

	var missing []string
	for i, ok := range found {
		if !ok {
			missing = append(missing, markers[i])
		}
	}
	return missing, nil
}
