package kubevirtenv

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The manifest URLs in this package are format strings consumed with
// fmt.Sprintf at their call sites. That makes a specific mistake easy and
// silent: drop the %s while editing a URL and the version is never
// interpolated, so the e2e cluster installs from a URL that is merely wrong
// rather than obviously broken, and the failure surfaces much later as a
// download error inside BeforeAll.
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
	for name, raw := range map[string]string{
		"CDIOperatorURL": CDIOperatorURL,
		"CDICRURL":       CDICRURL,
	} {
		got := fmt.Sprintf(raw, CDIVersion)

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
