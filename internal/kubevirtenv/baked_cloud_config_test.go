package kubevirtenv

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The e2e used to learn that the live installer never ran only from a 50 minute
// KairosControlPlane timeout reported as WaitingForNodePush, which points at
// node networking rather than at the install that never started
// (kairos-io/kairos#4991). These tests pin the cheap check that replaces it.
// They build synthetic images instead of real ISOs: the property under test is
// that a byte scan finds the baked cloud-config wherever it sits in a large
// file, which does not need ISO9660 structure.

// isoLike wraps payload in filler so the markers do not sit at offset zero and
// the scan has to cross several chunk boundaries to reach them.
func isoLike(t *testing.T, payload string, leading, trailing int) string {
	t.Helper()
	rnd := rand.New(rand.NewSource(1))
	filler := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			// Printable bytes only, so an accidental match is a real match.
			b[i] = byte('a' + rnd.Intn(26))
		}
		return b
	}
	var buf bytes.Buffer
	buf.Write(filler(leading))
	buf.WriteString(payload)
	buf.Write(filler(trailing))

	path := filepath.Join(t.TempDir(), "kairos-kubevirt.iso")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing synthetic image: %v", err)
	}
	return path
}

func TestVerifyBakedCloudConfigAcceptsAnImageCarryingTheConfig(t *testing.T) {
	// Spans several chunks so the happy path exercises the same carry-over
	// logic as the boundary test below.
	path := isoLike(t, installerCloudConfig, bakedScanChunk+7, 3*bakedScanChunk+11)
	if err := verifyBakedCloudConfig(path); err != nil {
		t.Fatalf("expected the image to pass, got: %v", err)
	}
}

func TestVerifyBakedCloudConfigRejectsAnImageWithoutTheConfig(t *testing.T) {
	// This is the shape the operator actually published: a valid, bootable,
	// full-size image that simply has no cloud-config in it.
	path := isoLike(t, "", bakedScanChunk, bakedScanChunk)
	err := verifyBakedCloudConfig(path)
	if err == nil {
		t.Fatal("expected an image with no baked cloud-config to be rejected")
	}
	if !errors.Is(err, errNoBakedCloudConfig) {
		t.Fatalf("expected errNoBakedCloudConfig, got: %v", err)
	}
	// The message has to name what is missing, otherwise it is the same kind
	// of unhelpful report this check exists to replace.
	for _, marker := range bakedCloudConfigMarkers {
		if !strings.Contains(err.Error(), marker) {
			t.Errorf("error does not report the missing marker %q: %v", marker, err)
		}
	}
}

func TestVerifyBakedCloudConfigRejectsAPartiallyBakedConfig(t *testing.T) {
	// A config that installs unattended but onto no device is still broken,
	// so every marker has to be required independently.
	for _, missing := range bakedCloudConfigMarkers {
		t.Run(missing, func(t *testing.T) {
			partial := strings.Replace(installerCloudConfig, missing, "", 1)
			if strings.Contains(partial, missing) {
				t.Fatalf("test setup did not remove %q", missing)
			}
			path := isoLike(t, partial, 1024, 1024)
			err := verifyBakedCloudConfig(path)
			if err == nil {
				t.Fatalf("expected rejection when %q is absent", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error does not name the absent marker %q: %v", missing, err)
			}
		})
	}
}

func TestScanForMarkersFindsAMarkerStraddlingAChunkBoundary(t *testing.T) {
	// The scan reads in fixed chunks. Without the carried-over tail a marker
	// split across two reads is invisible, which would fail a perfectly good
	// image. Place each marker so exactly one byte of it lands in the first
	// chunk.
	for _, marker := range bakedCloudConfigMarkers {
		t.Run(marker, func(t *testing.T) {
			// The first read fills overlap+bakedScanChunk bytes, where overlap
			// is len(marker)-1 for a single-marker scan. Start the marker one
			// byte before that window ends, so all but its first byte falls in
			// the next read.
			lead := (len(marker) - 1) + bakedScanChunk - 1
			path := isoLike(t, marker, lead, 128)
			missing, err := scanForMarkers(mustOpen(t, path), []string{marker})
			if err != nil {
				t.Fatalf("scan failed: %v", err)
			}
			if len(missing) != 0 {
				t.Fatalf("marker split across a chunk boundary was not found: %v", missing)
			}
		})
	}
}

func TestScanForMarkersReportsMissingMarkersInOrder(t *testing.T) {
	r := strings.NewReader("only the middle one is here: beta")
	missing, err := scanForMarkers(r, []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(missing) != 2 || missing[0] != "alpha" || missing[1] != "gamma" {
		t.Fatalf("expected [alpha gamma], got %v", missing)
	}
}

func TestScanForMarkersHandlesAnEmptyReader(t *testing.T) {
	missing, err := scanForMarkers(strings.NewReader(""), bakedCloudConfigMarkers)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(missing) != len(bakedCloudConfigMarkers) {
		t.Fatalf("expected every marker missing, got %v", missing)
	}
}

func TestVerifyBakedCloudConfigReportsAMissingFile(t *testing.T) {
	err := verifyBakedCloudConfig(filepath.Join(t.TempDir(), "absent.iso"))
	if err == nil {
		t.Fatal("expected an error for a missing image")
	}
	if errors.Is(err, errNoBakedCloudConfig) {
		t.Fatalf("a missing file must not be reported as a missing bake: %v", err)
	}
}

// TestInstallerCloudConfigContainsEveryMarker keeps the markers honest: if the
// cloud-config is edited so a marker no longer appears in it, the check would
// reject every image forever.
func TestInstallerCloudConfigContainsEveryMarker(t *testing.T) {
	for _, marker := range bakedCloudConfigMarkers {
		if !strings.Contains(installerCloudConfig, marker) {
			t.Errorf("installerCloudConfig no longer contains the marker %q", marker)
		}
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
