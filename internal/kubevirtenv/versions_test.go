package kubevirtenv

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The manifest URLs in this package are format strings taking a pinned
// version. That makes a specific mistake easy and silent: drop the %s while
// editing a URL and the version is never interpolated, so the e2e cluster
// installs from a URL that is merely wrong rather than obviously broken, and
// the failure surfaces much later as a download error inside BeforeAll.
//
// These tests pin the shape of the constants, not upstream's behaviour: they
// make no network calls and will not start failing because a project cut a
// release.

func TestManifestURLsTakeExactlyOneVersionVerb(t *testing.T) {
	for name, raw := range map[string]string{
		"CDIOperatorURL":      CDIOperatorURL,
		"CDICRURL":            CDICRURL,
		"KubeVirtOperatorURL": KubeVirtOperatorURL,
		"KubeVirtCRURL":       KubeVirtCRURL,
		"CertManagerURL":      CertManagerURL,
		"CalicoManifestURL":   CalicoManifestURL,
	} {
		if got := strings.Count(raw, "%s"); got != 1 {
			t.Errorf("%s has %d %%s verbs, want exactly 1: a missing verb drops the version silently, a second one corrupts the URL (%q)", name, got, raw)
		}
	}
}

func TestPinnedVersionsAreReleasesNotFloatingTags(t *testing.T) {
	// Every manifest this package installs is pinned to a named release; nothing
	// here may reintroduce a "latest" URL (CLAUDE.md rule 4 names this package).
	semver := regexp.MustCompile(`^v\d+\.\d+\.\d+`)
	for name, v := range map[string]string{
		"CDIVersion":         CDIVersion,
		"KubeVirtVersion":    KubeVirtVersion,
		"CertManagerVersion": CertManagerVersion,
		"CalicoVersion":      CalicoVersion,
	} {
		if !semver.MatchString(v) {
			t.Errorf("%s = %q, want a vX.Y.Z release tag", name, v)
		}
	}

	for name, raw := range map[string]string{
		"CDIOperatorURL":       CDIOperatorURL,
		"CDICRURL":             CDICRURL,
		"KubeVirtOperatorURL":  KubeVirtOperatorURL,
		"KubeVirtCRURL":        KubeVirtCRURL,
		"CertManagerURL":       CertManagerURL,
		"CalicoManifestURL":    CalicoManifestURL,
		"LocalPathManifestURL": LocalPathManifestURL,
	} {
		if strings.Contains(raw, "/latest/") {
			t.Errorf("%s uses a floating latest URL (%q); pin it to a release so an upstream publish cannot change what CI installs", name, raw)
		}
	}
}

func TestCDIURLsResolveToThePinnedRelease(t *testing.T) {
	// Exercises the helpers the installer actually calls, so this covers the
	// formatting as it is used rather than re-implementing it here.
	for name, got := range map[string]string{
		"CDIOperatorManifestURL": CDIOperatorManifestURL(),
		"CDICRManifestURL":       CDICRManifestURL(),
	} {
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("%s formatted to an unparseable URL %q: %v", name, got, err)
			continue
		}
		if u.Scheme != "https" {
			t.Errorf("%s formatted to scheme %q, want https", name, u.Scheme)
		}
		if !strings.Contains(got, "/"+CDIVersion+"/") {
			t.Errorf("%s = %q, want the pinned version %q as a path segment", name, got, CDIVersion)
		}
		if strings.Contains(got, "%!") {
			t.Errorf("%s = %q: fmt reported a formatting error, so the verb and argument disagree", name, got)
		}
	}
}

// TestPulledImagesArePinned covers the container images this package makes the
// e2e cluster pull, which the URL checks above do not reach. A floating tag
// here has the same consequence as a floating manifest URL and is harder to
// see: two runs of the same commit install different software, so a red run
// cannot be attributed to the diff that preceded it.
//
// Third-party images need a digest as well as a tag, because the tag itself is
// mutable in a registry we do not control. Images built locally and loaded into
// kind (DevControllerImageRef, KairosCAPIReleaseImage) are deliberately absent:
// their ":latest" is a name for a build output, never something CI pulls.
func TestPulledImagesArePinned(t *testing.T) {
	thirdParty := map[string]string{
		"KindNodeImage":     KindNodeImage,
		"ExporterCurlImage": ExporterCurlImage,
	}
	for name, ref := range thirdParty {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s = %q, want a @sha256: digest; the tag alone is mutable in a registry we do not control", name, ref)
		}
		// A digest under a floating tag still reads as "latest" to the next
		// person editing it, and the tag is what a bump gets written against.
		if imageTag(ref) == "latest" {
			t.Errorf("%s = %q, want a released tag next to the digest, not latest", name, ref)
		}
	}

	ours := map[string]string{
		"kairosOSArtifactBaseImage(amd64)": kairosOSArtifactBaseImage("amd64"),
		"kairosOSArtifactBaseImage(arm64)": kairosOSArtifactBaseImage("arm64"),
	}
	for name, ref := range ours {
		switch imageTag(ref) {
		case "":
			t.Errorf("%s = %q, want an explicit tag rather than the registry default", name, ref)
		case "latest":
			t.Errorf("%s = %q, want a released tag so a re-tag cannot change what CI installs", name, ref)
		}
	}
}

// imageTag returns the tag of a container image reference, ignoring any digest
// and any port in the registry host, and "" when the reference carries none.
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < slash {
		return ""
	}
	return ref[colon+1:]
}

// TestOSArtifactManifestCarriesThePinnedExporterImage exercises the manifest as
// it is rendered, so the pin cannot be reverted by editing the template alone.
func TestOSArtifactManifestCarriesThePinnedExporterImage(t *testing.T) {
	manifest := osArtifactManifest("amd64")
	if !strings.Contains(manifest, "image: "+ExporterCurlImage) {
		t.Errorf("rendered OSArtifact does not carry the pinned exporter image %q:\n%s", ExporterCurlImage, manifest)
	}
	if strings.Contains(manifest, "curlimages/curl:latest") {
		t.Errorf("rendered OSArtifact still pulls curlimages/curl:latest:\n%s", manifest)
	}
}
