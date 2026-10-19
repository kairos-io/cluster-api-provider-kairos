/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package k0sjointoken

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// The fixture under testdata/k0s-preshared-controller was produced by the real
// k0s binary at the documented baseline (v1.34.8+k0s.0, taken from the image
// quay.io/kairos/hadron:v0.4.0-standard-amd64-generic-v4.1.2-k0s-v1.34.8-k0s.0),
// with no network and a throwaway CA:
//
//	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
//	  -keyout ca.key -out ca.crt -days 1 -subj "/CN=kairos-capi-k0s-token-fixture-ca"
//	docker run --rm --network none --user "$(id -u):$(id -g)" \
//	  -v "$PWD:/in:ro" -v "$PWD/out:/out" --entrypoint /usr/bin/k0s <image> \
//	  token pre-shared --role controller --cert /in/ca.crt \
//	  --url https://192.0.2.10:9443 --valid 1s --out /out
//
// `token` is out/token_<id>, `bootstrap-token-secret.yaml` is
// out/bootstrap-token-<id>.yaml, and ca.key was discarded. The bootstrap token
// expired one second after it was created and was never applied to any
// cluster. Regenerate the fixture on every k0s baseline bump.
const (
	k0sFixtureDir    = "testdata/k0s-preshared-controller"
	k0sFixtureServer = "https://192.0.2.10:9443"
)

func readK0sFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(k0sFixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func fixtureCA(t *testing.T) []byte { return readK0sFixture(t, "ca.crt") }

// gunzipBase64 returns the kubeconfig inside an encoded k0s token.
func gunzipBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestEncode_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	tok, err := Generate()
	g.Expect(err).ToNot(HaveOccurred())
	ca := fixtureCA(t)

	for _, server := range []string{"https://10.0.0.5:9443", "https://[fd00::5]:9443"} {
		enc, err := Encode(server, ca, tok)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(enc).To(MatchRegexp(`^[A-Za-z0-9+/]+=*$`), "standard base64, the alphabet k0s uses")

		dec, err := Decode(enc)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(dec.Server).To(Equal(server))
		g.Expect(dec.CAData).To(Equal(ca))
		g.Expect(dec.User).To(Equal("controller-bootstrap"))
		g.Expect(dec.Token.Bearer()).To(MatchRegexp(`^[a-z0-9]{6}\.[a-z0-9]{16}$`))
		g.Expect(dec.Token).To(Equal(tok))
	}
}

// TestEncode_MatchesRealK0s compares our encoding with a
// token real k0s produced. The decoded kubeconfigs must be structurally equal
// when built from the same server, CA and bootstrap token.
func TestEncode_MatchesRealK0s(t *testing.T) {
	g := NewWithT(t)
	fixture := string(readK0sFixture(t, "token"))
	ca := fixtureCA(t)

	want, err := clientcmd.Load(gunzipBase64(t, fixture))
	g.Expect(err).ToNot(HaveOccurred())

	// Our decoder reads the real token.
	dec, err := Decode(fixture)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec.Server).To(Equal(k0sFixtureServer))
	g.Expect(dec.CAData).To(Equal(ca))
	g.Expect(dec.User).To(Equal(ControllerBootstrapUser))

	// Our encoder, given the same inputs, produces the same kubeconfig.
	ours, err := Encode(k0sFixtureServer, ca, dec.Token)
	g.Expect(err).ToNot(HaveOccurred())
	got, err := clientcmd.Load(gunzipBase64(t, ours))
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(got.CurrentContext).To(Equal(want.CurrentContext))
	g.Expect(got.CurrentContext).To(Equal("k0s"))
	g.Expect(keys(got.Clusters)).To(Equal(keys(want.Clusters)))
	g.Expect(keys(got.Contexts)).To(Equal(keys(want.Contexts)))
	g.Expect(keys(got.AuthInfos)).To(Equal(keys(want.AuthInfos)))
	for name, wc := range want.Clusters {
		g.Expect(got.Clusters[name].Server).To(Equal(wc.Server))
		g.Expect(got.Clusters[name].CertificateAuthorityData).To(Equal(wc.CertificateAuthorityData))
		g.Expect(got.Clusters[name].InsecureSkipTLSVerify).To(Equal(wc.InsecureSkipTLSVerify))
	}
	for name, wctx := range want.Contexts {
		g.Expect(got.Contexts[name].Cluster).To(Equal(wctx.Cluster))
		g.Expect(got.Contexts[name].AuthInfo).To(Equal(wctx.AuthInfo))
	}
	for name, wa := range want.AuthInfos {
		g.Expect(got.AuthInfos[name].Token).To(Equal(wa.Token))
	}

	// gzip at BestCompression has the same header k0s writes (no name, no mtime,
	// XFL=2 for maximum compression).
	rawOurs, _ := base64.StdEncoding.DecodeString(ours)
	rawFixture, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(fixture))
	g.Expect(rawOurs[:10]).To(Equal(rawFixture[:10]))
}

// TestNewSecret_MatchesRealK0s compares the workload
// bootstrap-token Secret with the one `k0s token pre-shared` writes.
func TestNewSecret_MatchesRealK0s(t *testing.T) {
	g := NewWithT(t)
	want := &corev1.Secret{}
	g.Expect(yaml.Unmarshal(readK0sFixture(t, "bootstrap-token-secret.yaml"), want)).To(Succeed())

	tok, err := Generate()
	g.Expect(err).ToNot(HaveOccurred())
	got := NewSecret(tok, time.Now().Add(time.Hour))

	g.Expect(got.Type).To(Equal(want.Type))
	g.Expect(got.Namespace).To(Equal(want.Namespace))
	g.Expect(got.Name).To(HavePrefix("bootstrap-token-"))
	g.Expect(want.Name).To(Equal("bootstrap-token-" + string(want.Data["token-id"])))
	g.Expect(got.Name).To(Equal("bootstrap-token-" + tok.ID()))
	g.Expect(keys(got.Data)).To(Equal(keys(want.Data)), "same data keys as k0s's own controller token")
	for _, usage := range []string{"usage-bootstrap-controller-join", "usage-controller-join"} {
		g.Expect(string(got.Data[usage])).To(Equal(string(want.Data[usage])))
	}
	_, err = time.Parse(time.RFC3339, string(want.Data["expiration"]))
	g.Expect(err).ToNot(HaveOccurred())
}

func TestNewSecret_Shape(t *testing.T) {
	g := NewWithT(t)
	tok := mustToken(t, "abcdef", "0123456789abcdef")
	expires := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	s := NewSecret(tok, expires)

	g.Expect(s.Namespace).To(Equal("kube-system"))
	g.Expect(s.Name).To(Equal("bootstrap-token-abcdef"))
	g.Expect(s.Type).To(Equal(corev1.SecretType("bootstrap.kubernetes.io/token")))
	g.Expect(s.Data).To(Equal(map[string][]byte{
		"token-id":                        []byte("abcdef"),
		"token-secret":                    []byte("0123456789abcdef"),
		"expiration":                      []byte("2026-10-16T13:00:00Z"),
		"description":                     []byte("k0s controller-join token minted by cluster-api-provider-kairos"),
		"usage-bootstrap-controller-join": []byte("true"),
		"usage-controller-join":           []byte("true"),
	}))
	// The token must not be able to authenticate to kube-apiserver at all.
	for _, forbidden := range []string{"usage-bootstrap-authentication", "usage-bootstrap-signing", "auth-extra-groups"} {
		g.Expect(s.Data).ToNot(HaveKey(forbidden))
	}
	g.Expect(IsControllerJoinOnly(s)).To(BeTrue())

	// The guard used before extending, reusing or deleting rejects anything else.
	auth := s.DeepCopy()
	auth.Data["usage-bootstrap-authentication"] = []byte("true")
	g.Expect(IsControllerJoinOnly(auth)).To(BeFalse())
	groups := s.DeepCopy()
	groups.Data["auth-extra-groups"] = []byte("system:bootstrappers:kubeadm:default-node-token")
	g.Expect(IsControllerJoinOnly(groups)).To(BeFalse())
	noJoin := s.DeepCopy()
	delete(noJoin.Data, "usage-bootstrap-controller-join")
	delete(noJoin.Data, "usage-controller-join")
	g.Expect(IsControllerJoinOnly(noJoin)).To(BeFalse())
	opaque := s.DeepCopy()
	opaque.Type = corev1.SecretTypeOpaque
	g.Expect(IsControllerJoinOnly(opaque)).To(BeFalse())
}

func TestGenerate(t *testing.T) {
	g := NewWithT(t)
	re := regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := Generate()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(re.MatchString(tok.Bearer())).To(BeTrue(), tok.String())
		g.Expect(seen[tok.Bearer()]).To(BeFalse())
		seen[tok.Bearer()] = true
	}
	tok := mustToken(t, "abcdef", "0123456789abcdef")
	g.Expect(tok.String()).To(Equal("abcdef.<redacted>"))
	g.Expect(fmt.Sprintf("%v %s %+v", tok, tok, tok)).ToNot(ContainSubstring("0123456789abcdef"))
}

func TestDecode_RejectsGarbageWithoutEchoingIt(t *testing.T) {
	g := NewWithT(t)
	gz := func(s string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	for name, in := range map[string]string{
		"not base64":     "!!!SECRETVALUE!!!",
		"not gzip":       base64.StdEncoding.EncodeToString([]byte("SECRETVALUE")),
		"not kubeconfig": gz("SECRETVALUE: [unclosed"),
		"no context":     gz("apiVersion: v1\nkind: Config\nusers:\n- name: u\n  user:\n    token: SECRETVALUE\n"),
		"bad bearer": gz("apiVersion: v1\nkind: Config\ncurrent-context: k0s\nclusters:\n- name: k0s\n  cluster:\n    server: https://10.0.0.1:9443\n" +
			"contexts:\n- name: k0s\n  context:\n    cluster: k0s\n    user: controller-bootstrap\nusers:\n- name: controller-bootstrap\n  user:\n    token: SECRETVALUE\n"),
	} {
		_, err := Decode(in)
		g.Expect(err).To(HaveOccurred(), name)
		g.Expect(err.Error()).ToNot(ContainSubstring("SECRETVALUE"), name)
	}
}

func mustToken(t *testing.T, id, secret string) BootstrapToken {
	t.Helper()
	tok, err := NewBootstrapToken(id, secret)
	if err != nil {
		t.Fatalf("token %s: %v", id, err)
	}
	return tok
}

func TestNewBootstrapToken_Validates(t *testing.T) {
	g := NewWithT(t)
	tok, err := NewBootstrapToken("abcdef", "0123456789abcdef")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tok.ID()).To(Equal("abcdef"))
	g.Expect(tok.Secret()).To(Equal("0123456789abcdef"))
	g.Expect(tok.Bearer()).To(Equal("abcdef.0123456789abcdef"))
	for _, bad := range [][2]string{{"ABCDEF", "0123456789abcdef"}, {"abcde", "0123456789abcdef"}, {"abcdef", "short"}, {"abc.de", "0123456789abcdef"}} {
		_, err := NewBootstrapToken(bad[0], bad[1])
		g.Expect(err).To(HaveOccurred(), "%v", bad)
		g.Expect(err.Error()).ToNot(ContainSubstring(bad[1]))
	}
	g.Expect(ValidID("abc123")).To(BeTrue())
	g.Expect(ValidID("abc12")).To(BeFalse())
	g.Expect(ValidID("../etc")).To(BeFalse())
	g.Expect(SecretKeyFor("abc123").String()).To(Equal("kube-system/bootstrap-token-abc123"))
}

func TestExpiry(t *testing.T) {
	g := NewWithT(t)
	s := NewSecret(mustToken(t, "abcdef", "0123456789abcdef"), time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC))
	got, ok := Expiry(s)
	g.Expect(ok).To(BeTrue())
	g.Expect(got).To(Equal(time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)))
	delete(s.Data, ExpirationKey)
	_, ok = Expiry(s)
	g.Expect(ok).To(BeFalse())
	s.Data[ExpirationKey] = []byte("tomorrow")
	_, ok = Expiry(s)
	g.Expect(ok).To(BeFalse())
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
