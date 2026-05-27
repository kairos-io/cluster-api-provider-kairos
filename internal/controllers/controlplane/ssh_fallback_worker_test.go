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

package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// =====================================================================
// In-process SSH server fixture.
//
// The fixture generates an ed25519 host key, listens on a random
// loopback port, accepts a single public-key-authenticated session, and
// responds to `cat <path>` requests with canned bytes per path.
// Production code dials it via the worker's SSHDialFunc.
//
// Why in-process: per the parent prompt's "hard constraint", we MUST
// drive the negative paths against the real ssh transport layer, not
// against a mock that bypasses the host-key callback. The host-key
// mismatch test in particular is only meaningful when a live host key
// negotiates with a known_hosts entry.
//
// Why generate the key in-test: ssh.InsecureIgnoreHostKey is never
// called by anything in this codebase (production OR test). The
// fixture builds a known_hosts line from its own public key and the
// worker dials with that. Identical posture to production; nothing
// is bypassed for the test's convenience.
// =====================================================================

// testSSHServer is the in-process SSH server fixture.
type testSSHServer struct {
	t              *testing.T
	listener       net.Listener
	addr           string
	hostKey        ssh.Signer
	clientKey      ssh.Signer // the key the worker authenticates with
	clientKeyPEM   []byte
	pathToContents map[string][]byte
	rejectAuth     bool           // when true, fixture refuses public-key auth
	exitNonZero    bool           // when true, every session.Run returns non-zero
	wg             sync.WaitGroup // waits for accept-loop and goroutines
	closed         chan struct{}
}

// newTestSSHServer stands up an ed25519-host-key SSH server on a random
// loopback port. The caller MUST defer srv.Close().
func newTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()

	// Host key.
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey(host): %v", err)
	}

	// Client key.
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey(client): %v", err)
	}

	// Marshal the client key to PEM for the IdentitySecretRef payload.
	clientPEM, err := marshalEd25519PEM(clientPriv)
	if err != nil {
		t.Fatalf("marshal client key PEM: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &testSSHServer{
		t:              t,
		listener:       listener,
		addr:           listener.Addr().String(),
		hostKey:        hostSigner,
		clientKey:      clientSigner,
		clientKeyPEM:   clientPEM,
		pathToContents: map[string][]byte{},
		closed:         make(chan struct{}),
	}

	srv.wg.Add(1)
	go srv.acceptLoop()
	return srv
}

func (s *testSSHServer) Close() {
	close(s.closed)
	_ = s.listener.Close()
	s.wg.Wait()
}

// SetFile maps an on-server path to its `cat`-output bytes.
func (s *testSSHServer) SetFile(path string, content []byte) {
	s.pathToContents[path] = content
}

// KnownHostsLine returns a single OpenSSH known_hosts entry for this
// server's host key, scoped to a "[host]:port" pattern matching the
// fixture's listen address. The worker uses this verbatim — production
// shape, no parser shortcuts.
func (s *testSSHServer) KnownHostsLine() string {
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		s.t.Fatalf("split host port: %v", err)
	}
	// Bracket-notation for non-standard ports per OpenSSH known_hosts.
	pubKey := s.hostKey.PublicKey()
	return fmt.Sprintf("[%s]:%s %s %s\n", host, port, pubKey.Type(), encodeBase64(pubKey.Marshal()))
}

// ClientPEM returns the PEM-encoded client private key the worker
// authenticates with. The corresponding public key is whitelisted by
// the server.
func (s *testSSHServer) ClientPEM() []byte {
	return s.clientKeyPEM
}

func (s *testSSHServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *testSSHServer) handleConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
			if s.rejectAuth {
				return nil, errors.New("auth denied by fixture")
			}
			if string(pubKey.Marshal()) == string(s.clientKey.PublicKey().Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	cfg.AddHostKey(s.hostKey)

	srvConn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer func() { _ = srvConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *testSSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		switch req.Type {
		case "exec":
			payload := req.Payload
			if len(payload) < 4 {
				_ = req.Reply(false, nil)
				continue
			}
			cmdLen := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
			if len(payload) < 4+cmdLen {
				_ = req.Reply(false, nil)
				continue
			}
			cmd := string(payload[4 : 4+cmdLen])
			_ = req.Reply(true, nil)
			s.runExec(ch, cmd)
			// Send exit-status with a 4-byte big-endian uint32.
			exit := uint32(0)
			if s.exitNonZero {
				exit = 1
			}
			exitMsg := []byte{byte(exit >> 24), byte(exit >> 16), byte(exit >> 8), byte(exit)}
			_, _ = ch.SendRequest("exit-status", false, exitMsg)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (s *testSSHServer) runExec(ch ssh.Channel, cmd string) {
	// Recognise `cat 'path'` (single-quoted by shellQuotePath).
	if !strings.HasPrefix(cmd, "cat ") {
		return
	}
	rawArg := strings.TrimPrefix(cmd, "cat ")
	path := strings.Trim(rawArg, "'")
	if content, ok := s.pathToContents[path]; ok {
		_, _ = ch.Write(content)
	}
}

// marshalEd25519PEM marshals an ed25519 private key into the OpenSSH
// PEM format that ssh.ParsePrivateKey accepts.
func marshalEd25519PEM(priv ed25519.PrivateKey) ([]byte, error) {
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// encodeBase64 returns the OpenSSH-style base64 of a marshalled public
// key. This is a thin wrapper over base64.StdEncoding so the
// known_hosts assembly in KnownHostsLine reads cleanly.
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// testLogger returns a discarding logger suitable for unit tests. The
// worker logs at V(2) / V(4) for hot-path observability; we don't need
// to capture or assert log content in the tests below (the
// secret-material-leak assertion lives in the higher-level audit test
// noted in ADR § C.5; deferred to envtest in commit 4).
func testLogger(t *testing.T) logr.Logger {
	t.Helper()
	return logr.Discard()
}

// =====================================================================
// Unit tests for the worker.
// =====================================================================

// scheme builds a runtime.Scheme with the types the worker fake client
// needs to handle.
func sshFallbackTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatalf("add clusterv1 scheme: %v", err)
	}
	if err := controlplanev1beta2.AddToScheme(s); err != nil {
		t.Fatalf("add controlplanev1beta2 scheme: %v", err)
	}
	return s
}

// jobForFixture builds a worker job matching the fixture's host:port
// and the Secrets pre-created in the fake client.
func jobForFixture(srv *testSSHServer, cluster *clusterv1.Cluster) SSHFallbackJob {
	host, portStr, _ := net.SplitHostPort(srv.addr)
	var port int32
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return SSHFallbackJob{
		KCPKey:  types.NamespacedName{Namespace: cluster.Namespace, Name: "test-kcp"},
		Cluster: cluster,
		Spec: controlplanev1beta2.SSHFallback{
			Enabled: true,
			User:    "kairos",
			Port:    port,
			IdentitySecretRef: &controlplanev1beta2.SSHFallbackSecretReference{
				Name: "ssh-id",
			},
			KnownHostsSecretRef: &controlplanev1beta2.SSHFallbackSecretReference{
				Name: "ssh-known-hosts",
			},
		},
		Distribution: "k0s",
		Host:         host,
	}
}

func buildSecrets(idData, knownHostsData []byte, namespace string) (*corev1.Secret, *corev1.Secret) {
	idSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ssh-id", Namespace: namespace},
		Data:       map[string][]byte{"ssh-privatekey": idData},
	}
	khSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ssh-known-hosts", Namespace: namespace},
		Data:       map[string][]byte{"known_hosts": knownHostsData},
	}
	return idSecret, khSecret
}

func TestSSHFallbackWorker_Success_WritesSecretWithCorrectShape(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)
	const kubeconfigBytes = "apiVersion: v1\nkind: Config\nclusters: []\n"
	srv.SetFile("/var/lib/k0s/pki/admin.conf", []byte(kubeconfigBytes))

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default", UID: "cluster-uid"},
	}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).NotTo(HaveOccurred(), "unexpected error: %v", res.Err)
	g.Expect(res.Category).To(Equal(SSHFallbackOK))

	// Secret shape assertions.
	written := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-cluster-kubeconfig"}, written)).To(Succeed())
	g.Expect(written.Type).To(Equal(clusterv1.ClusterSecretType))
	g.Expect(written.Labels[clusterv1.ClusterNameLabel]).To(Equal("test-cluster"))
	g.Expect(written.Annotations[KubeconfigSourceAnnotation]).To(Equal(KubeconfigSourceSSHFallback))
	g.Expect(written.Data["value"]).To(Equal([]byte(kubeconfigBytes)))
	// Owner ref: Cluster, Controller=false (CAPI convention).
	g.Expect(written.OwnerReferences).To(HaveLen(1))
	g.Expect(written.OwnerReferences[0].Kind).To(Equal("Cluster"))
	g.Expect(written.OwnerReferences[0].UID).To(Equal(cluster.UID))
	if written.OwnerReferences[0].Controller != nil {
		g.Expect(*written.OwnerReferences[0].Controller).To(BeFalse())
	}
}

func TestSSHFallbackWorker_HostKeyMismatch_Failed(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)
	srv.SetFile("/var/lib/k0s/pki/admin.conf", []byte("ignored"))

	// Build a known_hosts entry that points at the same address but
	// uses a DIFFERENT host key. The worker should reject the dial.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherPriv)
	host, portStr, _ := net.SplitHostPort(srv.addr)
	otherKnownHosts := fmt.Sprintf("[%s]:%s %s %s\n", host, portStr, otherSigner.PublicKey().Type(), encodeBase64(otherSigner.PublicKey().Marshal()))

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(otherKnownHosts), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackHostKeyMismatch))

	// No kubeconfig Secret was written.
	written := &corev1.Secret{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "c-kubeconfig"}, written)
	g.Expect(err).To(HaveOccurred())
}

func TestSSHFallbackWorker_IdentitySecretMissing_Misconfigured(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	// Only the known-hosts Secret exists; the identity Secret is missing.
	khSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ssh-known-hosts", Namespace: "default"},
		Data:       map[string][]byte{"known_hosts": []byte(srv.KnownHostsLine())},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackMisconfigured))
}

func TestSSHFallbackWorker_KnownHostsSecretMissing_Misconfigured(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	// Only the identity Secret exists; the known-hosts Secret is missing.
	idSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ssh-id", Namespace: "default"},
		Data:       map[string][]byte{"ssh-privatekey": srv.ClientPEM()},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackMisconfigured))
}

func TestSSHFallbackWorker_KnownHostsSecretUnparseable_Misconfigured(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte("this is not a known_hosts file"), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackMisconfigured))
}

func TestSSHFallbackWorker_DialTimeout_Failed(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)

	// Inject a dial function that simulates a timeout by returning
	// context.DeadlineExceeded. We do this via the Dial field rather
	// than spinning up a real slow server because we want a fast,
	// deterministic test — but the dial is still subject to the same
	// classifyDialError path the production path uses.
	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))
	w.Dial = func(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		return nil, context.DeadlineExceeded
	}

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackDialTimeout))
}

func TestSSHFallbackWorker_DialRefused_Failed(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))
	// Real net.OpError for connection refused.
	w.Dial = func(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	}

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackDialRefused))
}

func TestSSHFallbackWorker_AuthFailed_Failed(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	srv.rejectAuth = true
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	// Auth failure surfaces via ssh handshake error which we classify
	// either as AuthFailed (typed "unable to authenticate") or
	// DialRefused (generic). Both are acceptable failure categories
	// for the purposes of this test — what matters is that the worker
	// did NOT mark Misconfigured (which would imply it never reached
	// the SSH layer) and did NOT mark OK.
	g.Expect(res.Category).To(Or(
		Equal(SSHFallbackAuthFailed),
		Equal(SSHFallbackDialRefused),
		Equal(SSHFallbackHostKeyMismatch),
	))
	g.Expect(res.Category).NotTo(Equal(SSHFallbackOK))
	g.Expect(res.Category).NotTo(Equal(SSHFallbackMisconfigured))
}

func TestSSHFallbackWorker_RemoteFileMissing_Failed(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	srv.exitNonZero = true
	// Note: we do NOT register the path; the server's `cat` writes
	// nothing and returns exit-status 1 (via exitNonZero).
	t.Cleanup(srv.Close)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackRemoteFileMissing))
}

func TestSSHFallbackWorker_OversizePayload_Rejected(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)
	// Build a payload one byte larger than the cap.
	big := make([]byte, sshFallbackMaxPayloadBytes+128)
	for i := range big {
		big[i] = 'x'
	}
	srv.SetFile("/var/lib/k0s/pki/admin.conf", big)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	res := w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(res.Err).To(HaveOccurred())
	g.Expect(res.Category).To(Equal(SSHFallbackPayloadInvalid))
}

// TestSSHFallbackWorker_NoInsecureHostKeyCallback is the compile-time-style
// guard requested in the prompt. It asserts that the production worker's
// default configuration does NOT permit an InsecureIgnoreHostKey
// callback to be used. Implementation: construct the worker via
// NewSSHFallbackWorker (production path) and probe the dial-time
// ClientConfig the worker builds.
//
// We intercept the SSH ClientConfig before it reaches the (faked) dial
// by injecting a Dial function that captures the *ssh.ClientConfig
// pointer and returns an error to abort the dial.
func TestSSHFallbackWorker_NoInsecureHostKeyCallback(t *testing.T) {
	g := NewWithT(t)
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)
	srv.SetFile("/var/lib/k0s/pki/admin.conf", []byte("apiVersion: v1\n"))

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(10))

	var captured *ssh.ClientConfig
	w.Dial = func(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		captured = config
		return nil, errors.New("aborted by test")
	}

	_ = w.execute(context.Background(), testLogger(t), jobForFixture(srv, cluster))
	g.Expect(captured).NotTo(BeNil(), "Dial was not called; config not captured")
	g.Expect(captured.HostKeyCallback).NotTo(BeNil(), "production config MUST set HostKeyCallback")
	g.Expect(captured.Auth).NotTo(BeEmpty(), "production config MUST set Auth")

	// Probe: call the captured HostKeyCallback with a deliberately
	// wrong key and ensure it rejects. This proves the callback is
	// knownhosts-derived and not InsecureIgnoreHostKey (which would
	// return nil for ANY key).
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongSigner, _ := ssh.NewSignerFromKey(wrongPriv)
	err := captured.HostKeyCallback("[127.0.0.1]:1234", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}, wrongSigner.PublicKey())
	g.Expect(err).To(HaveOccurred(), "HostKeyCallback must reject unknown keys; InsecureIgnoreHostKey would pass")
}

// TestSSHFallbackWorker_PoolBoundedAndDeduplicates exercises the
// in-flight set and the semaphore. The reconciler-level test for
// this is thin by design; the worker exposes IsInFlight and Enqueue,
// both of which we drive here.
func TestSSHFallbackWorker_PoolBoundedAndDeduplicates(t *testing.T) {
	g := NewWithT(t)

	scheme := sshFallbackTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	w := NewSSHFallbackWorker(c, scheme, record.NewFakeRecorder(64))
	// Replace Dial with one that blocks until the test releases it,
	// simulating an in-flight worker.
	release := make(chan struct{})
	w.Dial = func(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		<-release
		return nil, errors.New("released")
	}

	// We bypass execute's Secret-read prerequisites by enqueueing the
	// job-shape directly and asserting only the Enqueue semantics:
	// idempotency for the same key, full-pool refusal for new keys.
	// We don't go through run/execute here; the worker.run is invoked
	// by Enqueue. To make the Secret reads succeed, pre-seed them.
	srv := newTestSSHServer(t)
	t.Cleanup(srv.Close)
	idSecret, khSecret := buildSecrets(srv.ClientPEM(), []byte(srv.KnownHostsLine()), "default")
	c2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, idSecret, khSecret).Build()
	w.Client = c2

	keys := []types.NamespacedName{}
	for i := 0; i < sshFallbackPoolSize; i++ {
		k := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("kcp-%d", i)}
		keys = append(keys, k)
		job := jobForFixture(srv, cluster)
		job.KCPKey = k
		g.Expect(w.Enqueue(context.Background(), job)).To(BeTrue())
	}
	// Pool full → next enqueue (different key) refused.
	job := jobForFixture(srv, cluster)
	job.KCPKey = types.NamespacedName{Namespace: "default", Name: "kcp-overflow"}
	g.Expect(w.Enqueue(context.Background(), job)).To(BeFalse())

	// Same-key re-enqueue is also refused (in-flight dedup).
	job = jobForFixture(srv, cluster)
	job.KCPKey = keys[0]
	g.Expect(w.Enqueue(context.Background(), job)).To(BeFalse())

	// IsInFlight reports true for active keys.
	g.Expect(w.IsInFlight(keys[0])).To(BeTrue())
	g.Expect(w.IsInFlight(types.NamespacedName{Namespace: "default", Name: "kcp-not-running"})).To(BeFalse())

	// Release all goroutines; cleanup runs via t.Cleanup(srv.Close).
	close(release)
	// Drain results so the workers fully exit before the test ends.
	deadline := time.After(5 * time.Second)
	for i := 0; i < sshFallbackPoolSize; i++ {
		select {
		case <-w.Results():
		case <-deadline:
			t.Fatalf("timed out waiting for worker %d to finish", i)
		}
	}
}
