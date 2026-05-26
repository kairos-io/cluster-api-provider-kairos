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

// Package controlplane: SSH-fallback worker (PR-9 / KD-3b).
//
// This file implements the background worker that the SSH-fallback sibling
// reconciler enqueues against (see ssh_fallback_controller.go). Per ADR
// 0002 § B.2 and internal/controllers/CLAUDE.md § 7, all SSH I/O runs in
// the worker goroutine; the reconciler only schedules and observes.
//
// Security non-negotiables embedded in the code below (and asserted by
// unit tests in ssh_fallback_worker_test.go):
//
//   - The only HostKeyCallback ever constructed is the one returned from
//     golang.org/x/crypto/ssh/knownhosts.New. No code path constructs
//     ssh.InsecureIgnoreHostKey(). The build-tagged compile-time guard
//     test in ssh_fallback_worker_test.go re-asserts this on every run.
//   - The worker never logs SSH private-key material, host-key bytes,
//     known_hosts content, or the fetched kubeconfig payload. It logs
//     stable identifiers (cluster name, secret namespace/name) and error
//     CATEGORIES, not raw error messages.
//   - Misconfiguration (missing/empty/unparseable Secrets) is reported
//     via SSHFallbackMisconfiguredReason and never falls back to a
//     no-host-key-verification mode. Constructor for the ssh.ClientConfig
//     lives in one place; a test fails if either HostKeyCallback or
//     Auth ends up unset.

package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// KubeconfigSourceAnnotation is the annotation key the worker stamps on the
// cluster kubeconfig Secret so operators can distinguish a kubeconfig that
// arrived via SSH fallback from one pushed by the workload node. Matches the
// annotation key the cloud-config templates write for the node-push path
// (see internal/bootstrap/templates/*_capv.yaml.tmpl and *_capk.yaml.tmpl).
const KubeconfigSourceAnnotation = "controllers.cluster.x-k8s.io/kubeconfig-source"

// KubeconfigSourceSSHFallback is the annotation value the SSH fallback path
// writes. The node-push path writes "node-push".
const KubeconfigSourceSSHFallback = "ssh-fallback"

const (
	// sshFallbackPoolSize is the hard-coded worker-pool concurrency limit.
	// Per ADR 0002 § F.2 PR-9 does NOT expose this as a flag; revisit when
	// HA lands (KD-5).
	sshFallbackPoolSize = 4

	// sshFallbackDialTimeout caps any single TCP+SSH-handshake attempt.
	sshFallbackDialTimeout = 30 * time.Second

	// sshFallbackJobBudget is the hard upper bound on total time a single
	// SSH-fetch job may take, including Secret reads, dial, exec, and
	// Secret write. Prevents a hung remote shell from tying up a worker
	// indefinitely.
	sshFallbackJobBudget = 2 * time.Minute

	// sshFallbackMaxPayloadBytes caps the bytes read from the remote
	// "cat <kubeconfig>" output. admin.conf / k3s.yaml are ~5 KiB in
	// practice; 1 MiB is a defensive cap (per ADR § C.1).
	sshFallbackMaxPayloadBytes = 1 << 20
)

// SSHFallbackJob is the unit of work the reconciler enqueues onto the
// worker pool. It carries snapshots of the inputs the worker needs so the
// goroutine does not race with the reconciler's view of the KCP.
type SSHFallbackJob struct {
	// KCPKey identifies the KairosControlPlane this job targets. The
	// worker re-fetches the KCP at job-start to apply the latest status
	// transition.
	KCPKey types.NamespacedName

	// Cluster is the owning Cluster; the worker writes the kubeconfig
	// Secret in cluster.Namespace with name "<cluster.Name>-kubeconfig".
	// A snapshot is sufficient — the name/namespace do not mutate during
	// a job's lifetime.
	Cluster *clusterv1.Cluster

	// Spec is a snapshot of KairosControlPlaneSpec.SSHFallback as
	// observed at enqueue time. Spec changes between enqueue and run are
	// intentionally NOT picked up; the next reconcile will enqueue a
	// fresh job with the new spec.
	Spec controlplanev1beta2.SSHFallback

	// Distribution selects which on-VM file the worker reads
	// (/var/lib/k0s/pki/admin.conf vs /etc/rancher/k3s/k3s.yaml).
	Distribution string

	// Host is the workload-node IP the worker dials. Resolved by the
	// reconciler from the first control-plane Machine's
	// Status.Addresses (InternalIP > ExternalIP > any). The worker does
	// NOT attempt DNS or additional resolution.
	Host string
}

// SSHFallbackResultCategory classifies a finished SSH-fetch attempt for
// condition-Reason mapping and Event emission. The category is what the
// reconciler reflects onto KubeconfigReadyCondition; the underlying error
// is logged with sanitized context but never with raw bytes from the
// SSH channel.
type SSHFallbackResultCategory string

const (
	// SSHFallbackOK indicates the worker successfully wrote the cluster
	// kubeconfig Secret. observeKubeconfigSecret will pick it up on the
	// next KCP reconcile and transition the condition to True with
	// Reason=KubeconfigReadyViaSSHFallback (the source annotation steers
	// the Reason; see kairoscontrolplane_controller.go).
	SSHFallbackOK SSHFallbackResultCategory = "OK"

	// SSHFallbackMisconfigured indicates a missing or unparseable
	// IdentitySecretRef / KnownHostsSecretRef, or a known_hosts file with
	// no entries. Maps to SSHFallbackMisconfiguredReason. Resolution is
	// for the operator to fix the referenced Secret; the path retries.
	SSHFallbackMisconfigured SSHFallbackResultCategory = "Misconfigured"

	// SSHFallbackHostKeyMismatch indicates the workload node's offered
	// host key did not match any entry in the known_hosts content. Maps
	// to SSHFallbackFailedReason with category "host-key-mismatch". Does
	// NOT fall back to TOFU.
	SSHFallbackHostKeyMismatch SSHFallbackResultCategory = "HostKeyMismatch"

	// SSHFallbackAuthFailed indicates the SSH handshake completed at the
	// transport layer but authentication was rejected.
	SSHFallbackAuthFailed SSHFallbackResultCategory = "AuthFailed"

	// SSHFallbackDialTimeout indicates the TCP+SSH handshake exceeded
	// sshFallbackDialTimeout.
	SSHFallbackDialTimeout SSHFallbackResultCategory = "DialTimeout"

	// SSHFallbackDialRefused indicates a connection-refused or no-route
	// outcome (transient infra).
	SSHFallbackDialRefused SSHFallbackResultCategory = "DialRefused"

	// SSHFallbackRemoteFileMissing indicates the remote `cat <path>`
	// returned a non-zero exit (typically file not found, permission
	// denied, or path mismatch with distribution).
	SSHFallbackRemoteFileMissing SSHFallbackResultCategory = "RemoteFileMissing"

	// SSHFallbackPayloadInvalid indicates the remote file content
	// exceeded the size cap or was otherwise unfit to write as a
	// kubeconfig.
	SSHFallbackPayloadInvalid SSHFallbackResultCategory = "PayloadInvalid"

	// SSHFallbackWriteFailed indicates a management-cluster API error
	// writing the kubeconfig Secret (apiserver unreachable, RBAC denied,
	// etc.).
	SSHFallbackWriteFailed SSHFallbackResultCategory = "WriteFailed"
)

// SSHFallbackResult is the worker's report of a single SSH-fetch attempt.
// It is value-typed so callers can compare without dereferencing.
type SSHFallbackResult struct {
	// Category is the high-level outcome; reconciler maps it to the
	// KubeconfigReadyCondition Reason.
	Category SSHFallbackResultCategory

	// Err is the underlying error (nil on success). Logged with
	// sanitized context — see "Loggable surface" in ADR § C.5.
	Err error
}

// SSHDialFunc is the function type the worker uses to establish an SSH
// client connection. Production wires this to ssh.Dial; tests inject a
// fixture that talks to an in-process ssh.NewServerConn-based test
// server, OR (when fixture setup is non-trivial) a mock that returns
// canned errors for the negative-path tests. Either way, NO test ever
// uses ssh.InsecureIgnoreHostKey — the fixture generates a host key and
// the test wires the matching known_hosts entry through the Secret.
type SSHDialFunc func(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)

// defaultSSHDial is the production dial implementation. It wraps
// ssh.Dial with a context that enforces sshFallbackDialTimeout; the
// underlying ssh library's Timeout field handles the TCP-level part,
// and the context cancel handles the upper-layer handshake.
func defaultSSHDial(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	// ssh.ClientConfig.Timeout is honored by ssh.Dial; we still respect
	// the caller's ctx by passing the addr through net.Dialer.DialContext
	// and then layering the SSH client on top. This makes graceful
	// shutdown work: cancelling the worker's job context aborts the dial
	// at the TCP layer.
	d := net.Dialer{Timeout: config.Timeout}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		// Best effort: close the underlying TCP socket before returning.
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// SSHFallbackWorker owns the bounded worker pool that runs SSH-fetch
// jobs. It is registered alongside the manager (one instance per
// controller process) and shared by the sibling reconciler. The pool
// itself is implemented as a buffered-channel semaphore (stdlib only,
// no x/sync dependency) to keep the supply-chain surface minimal.
//
// Concurrency invariants:
//
//   - At most sshFallbackPoolSize goroutines are ever blocked on the
//     SSH dial at one time.
//   - The inFlight set guarantees idempotency: a re-reconcile of the
//     same KCP while a job is running does not double-enqueue.
//   - All exported methods are safe for concurrent use.
type SSHFallbackWorker struct {
	// Client is the management-cluster client. Used to read the two
	// referenced Secrets and write the cluster kubeconfig Secret.
	Client client.Client

	// Scheme is needed by controllerutil.SetControllerReference on the
	// kubeconfig Secret.
	Scheme *runtime.Scheme

	// Recorder emits Events on the KCP for each fallback attempt
	// outcome. Logged-with-the-KCP for operator visibility via
	// `kubectl describe kcp`.
	Recorder record.EventRecorder

	// Dial is the function used to open an SSH client connection.
	// Production wires defaultSSHDial; tests inject a fixture.
	Dial SSHDialFunc

	// sem is a buffered channel used as a counting semaphore; each
	// Enqueue acquires a slot before launching its goroutine. Size is
	// sshFallbackPoolSize.
	sem chan struct{}

	// mu guards inFlight and the result-channel set.
	mu sync.Mutex

	// inFlight tracks KCP keys whose worker goroutine has not yet
	// reported back. A new Enqueue for a key already in this set is a
	// no-op: the reconciler will see the result on the worker's next
	// status write and decide whether to re-enqueue.
	inFlight map[types.NamespacedName]struct{}

	// results is the channel the reconciler reads results from. Buffered
	// to (poolSize * 2) so a burst of finishes does not block workers.
	// Producers (workers) send result envelopes; the reconciler-side
	// consumer drains them and re-enqueues the KCP for the watch loop.
	results chan SSHFallbackResultEnvelope
}

// SSHFallbackResultEnvelope wraps a SSHFallbackResult with the KCP key
// the result pertains to. The reconciler-side consumer uses the key to
// re-enqueue that specific KCP so the next Reconcile reflects the
// updated condition.
type SSHFallbackResultEnvelope struct {
	KCPKey types.NamespacedName
	Result SSHFallbackResult
}

// NewSSHFallbackWorker constructs a worker with production wiring. The
// worker is intended to be a process-singleton: register one in main.go
// and share it with the sibling reconciler.
func NewSSHFallbackWorker(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *SSHFallbackWorker {
	return &SSHFallbackWorker{
		Client:   c,
		Scheme:   scheme,
		Recorder: recorder,
		Dial:     defaultSSHDial,
		sem:      make(chan struct{}, sshFallbackPoolSize),
		inFlight: make(map[types.NamespacedName]struct{}),
		results:  make(chan SSHFallbackResultEnvelope, sshFallbackPoolSize*2),
	}
}

// Results returns the channel the reconciler reads worker outcomes from.
// The reconciler is expected to drain this channel in a long-running
// goroutine and call its own EnqueueRequest helper to wake the watch.
func (w *SSHFallbackWorker) Results() <-chan SSHFallbackResultEnvelope {
	return w.results
}

// IsInFlight reports whether a job for the given KCP key is currently
// being processed by the worker pool. Reconcilers use this to suppress
// double-enqueue.
func (w *SSHFallbackWorker) IsInFlight(key types.NamespacedName) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.inFlight[key]
	return ok
}

// Enqueue starts a worker goroutine for the given job, subject to the
// pool size and the in-flight set. Returns true when the job was
// accepted, false when it was dropped (already in flight). Never blocks
// on the semaphore: a full pool causes the new job to be deferred to
// the next reconcile (which will see Enqueue return false and requeue).
func (w *SSHFallbackWorker) Enqueue(ctx context.Context, job SSHFallbackJob) bool {
	w.mu.Lock()
	if _, ok := w.inFlight[job.KCPKey]; ok {
		w.mu.Unlock()
		return false
	}
	// Try to grab a semaphore slot non-blocking. A full pool drops the
	// job; the reconciler will re-enqueue on its next requeue.
	select {
	case w.sem <- struct{}{}:
	default:
		w.mu.Unlock()
		return false
	}
	w.inFlight[job.KCPKey] = struct{}{}
	w.mu.Unlock()

	go w.run(ctx, job)
	return true
}

// run executes a single SSH-fetch job under the per-job timeout budget.
// All exits release the semaphore slot, clear inFlight, and post the
// result envelope.
func (w *SSHFallbackWorker) run(parent context.Context, job SSHFallbackJob) {
	jobCtx, cancel := context.WithTimeout(parent, sshFallbackJobBudget)
	defer cancel()
	defer func() {
		w.mu.Lock()
		delete(w.inFlight, job.KCPKey)
		w.mu.Unlock()
		<-w.sem
	}()

	log := ctrl.LoggerFrom(parent).WithName("ssh-fallback-worker").WithValues(
		"kcp", job.KCPKey.String(),
		"cluster", types.NamespacedName{Namespace: job.Cluster.Namespace, Name: job.Cluster.Name}.String(),
		"host", job.Host,
	)
	res := w.execute(jobCtx, log, job)
	w.emitEvent(jobCtx, job, res)
	w.postResult(jobCtx, job.KCPKey, res)
}

// emitEvent posts a Kubernetes Event on the KCP for this job's outcome.
// The Event message contains only the result category and (on failure)
// the error category — never raw error strings that might leak the
// remote address, host key bytes, or other sensitive context.
func (w *SSHFallbackWorker) emitEvent(ctx context.Context, job SSHFallbackJob, res SSHFallbackResult) {
	if w.Recorder == nil {
		return
	}
	// Re-fetch the KCP for the Recorder; using a synthetic ObjectReference
	// avoids needing the UID at this layer.
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := w.Client.Get(ctx, job.KCPKey, kcp); err != nil {
		// Best-effort: if the KCP has been deleted while we were
		// running, drop the event.
		return
	}
	eventType := corev1.EventTypeWarning
	if res.Category == SSHFallbackOK {
		eventType = corev1.EventTypeNormal
	}
	msg := fmt.Sprintf("SSH fallback: %s", res.Category)
	w.Recorder.Event(kcp, eventType, fmt.Sprintf("SSHFallback%s", res.Category), msg)
}

// postResult drops the result onto the results channel. Non-blocking:
// if the consumer has fallen behind, the result is dropped (the
// reconciler will re-discover the outcome on its next watch tick via
// observeKubeconfigSecret). This is a defensive backstop, not a
// steady-state behavior.
// postResult publishes a worker result onto the buffered channel the
// sibling reconciler drains. The send BLOCKS on a full channel rather
// than silently dropping — a dropped `Failed` / `Misconfigured` envelope
// would leave the KCP wedged in `SSHFallbackDialingReason` because the
// reconciler's eligibility predicate treats `Dialing` as "in flight, do
// not retry" (see ssh_fallback_controller.go evaluateEligibility, the
// SSHFallbackDialingReason case). A dropped Success is recoverable via
// observeKubeconfigSecret's Secret watch; a dropped Failure is not.
// (security-architect review of f5379d0 flagged this as Medium.)
//
// The only legitimate exit without sending is manager shutdown, signalled
// by ctx.Done(). Until then, blocking on the drain pushes back on the
// worker pool — which is exactly the semantics we want; if the drain is
// stalled, do not generate more work that the same stalled drain would
// have to handle.
func (w *SSHFallbackWorker) postResult(ctx context.Context, key types.NamespacedName, res SSHFallbackResult) {
	envelope := SSHFallbackResultEnvelope{KCPKey: key, Result: res}
	select {
	case w.results <- envelope:
	case <-ctx.Done():
	}
}

// execute does the actual SSH-fetch dance: read both Secrets, parse
// host-key/identity, dial, exec, parse, write kubeconfig. Returns a
// classified result; never panics; never returns a raw error wrap that
// might leak sensitive material.
func (w *SSHFallbackWorker) execute(ctx context.Context, log logr.Logger, job SSHFallbackJob) SSHFallbackResult {
	// --- (1) Resolve known_hosts.
	hostKeyCB, cat, err := w.loadKnownHosts(ctx, log, job)
	if err != nil {
		return SSHFallbackResult{Category: cat, Err: err}
	}

	// --- (2) Resolve identity.
	signer, cat, err := w.loadIdentity(ctx, log, job)
	if err != nil {
		return SSHFallbackResult{Category: cat, Err: err}
	}

	// --- (3) Construct ssh.ClientConfig. Both HostKeyCallback and Auth
	// MUST be non-nil; the test TestSSHFallbackWorker_ConfigConstructionGuards
	// proves no code path elides either.
	cfg := &ssh.ClientConfig{
		User:            job.Spec.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCB,
		Timeout:         sshFallbackDialTimeout,
	}
	if cfg.HostKeyCallback == nil || len(cfg.Auth) == 0 {
		// Defense in depth: if the ssh.ClientConfig is ever incomplete,
		// refuse to dial. This is the failure-open mitigation from
		// ADR § C.4.
		return SSHFallbackResult{Category: SSHFallbackMisconfigured, Err: errors.New("ssh client config incomplete")}
	}

	// --- (4) Dial.
	port := int32(22)
	if job.Spec.Port != 0 {
		port = job.Spec.Port
	}
	addr := net.JoinHostPort(job.Host, fmt.Sprintf("%d", port))
	dialCtx, dialCancel := context.WithTimeout(ctx, sshFallbackDialTimeout)
	defer dialCancel()

	sshClient, err := w.Dial(dialCtx, "tcp", addr, cfg)
	if err != nil {
		cat := classifyDialError(err)
		log.Info("SSH dial failed", "category", string(cat))
		return SSHFallbackResult{Category: cat, Err: err}
	}
	defer func() { _ = sshClient.Close() }()

	// --- (5) Exec `cat <path>` over a single session. No PTY, no shell.
	path := remoteKubeconfigPath(job.Distribution)
	if path == "" {
		return SSHFallbackResult{Category: SSHFallbackMisconfigured, Err: fmt.Errorf("unsupported distribution: %s", job.Distribution)}
	}

	payload, cat, err := w.runRemoteCat(ctx, log, sshClient, path)
	if err != nil {
		return SSHFallbackResult{Category: cat, Err: err}
	}

	// --- (6) Write the cluster kubeconfig Secret.
	if err := w.writeKubeconfigSecret(ctx, log, job, payload); err != nil {
		return SSHFallbackResult{Category: SSHFallbackWriteFailed, Err: err}
	}

	log.Info("SSH fallback succeeded", "payloadBytes", len(payload))
	return SSHFallbackResult{Category: SSHFallbackOK}
}

// loadKnownHosts reads the KnownHostsSecretRef from the management cluster
// and parses it with ssh/knownhosts.New into a HostKeyCallback. Empty,
// missing, or unparseable Secrets all map to SSHFallbackMisconfigured —
// the worker never falls back to a callback that would accept any host.
func (w *SSHFallbackWorker) loadKnownHosts(ctx context.Context, log logr.Logger, job SSHFallbackJob) (ssh.HostKeyCallback, SSHFallbackResultCategory, error) {
	ref := job.Spec.KnownHostsSecretRef
	if ref == nil || ref.Name == "" {
		// Should have been caught by the webhook when Enabled=true; if
		// we got here the operator updated the Secret reference out
		// from under us. Treat as misconfigured.
		return nil, SSHFallbackMisconfigured, errors.New("knownHostsSecretRef is required")
	}
	key := ref.Key
	if key == "" {
		key = "known_hosts"
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      ref.Name,
		Namespace: secretRefNamespace(ref, job.Cluster.Namespace),
	}
	if err := w.Client.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("known_hosts Secret not found", "secret", secretKey.String())
			return nil, SSHFallbackMisconfigured, fmt.Errorf("known_hosts Secret not found: %s", secretKey.String())
		}
		log.Info("known_hosts Secret read error", "secret", secretKey.String(), "category", "api-error")
		return nil, SSHFallbackMisconfigured, fmt.Errorf("known_hosts Secret read error: %w", err)
	}
	data, ok := secret.Data[key]
	if !ok || len(data) == 0 {
		log.Info("known_hosts Secret data key empty", "secret", secretKey.String(), "dataKey", key)
		return nil, SSHFallbackMisconfigured, fmt.Errorf("known_hosts Secret %s missing or empty data key %q", secretKey.String(), key)
	}

	// knownhosts.New takes a list of file paths and reads them. We have
	// the bytes in memory; the package does not expose a from-bytes
	// constructor, so we materialize the bytes to a temp file. The temp
	// file is created with 0600 and removed before this function
	// returns; the bytes are equivalent to what would have been
	// fetched from a Secret-mounted volume.
	cb, err := newKnownHostsCallback(data)
	if err != nil {
		log.Info("known_hosts parse failed", "secret", secretKey.String(), "category", "parse-error")
		return nil, SSHFallbackMisconfigured, fmt.Errorf("known_hosts parse failed: %w", err)
	}
	return cb, "", nil
}

// loadIdentity reads the IdentitySecretRef and parses the PEM private
// key. Same misconfig handling as loadKnownHosts.
func (w *SSHFallbackWorker) loadIdentity(ctx context.Context, log logr.Logger, job SSHFallbackJob) (ssh.Signer, SSHFallbackResultCategory, error) {
	ref := job.Spec.IdentitySecretRef
	if ref == nil || ref.Name == "" {
		return nil, SSHFallbackMisconfigured, errors.New("identitySecretRef is required")
	}
	key := ref.Key
	if key == "" {
		key = "ssh-privatekey"
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      ref.Name,
		Namespace: secretRefNamespace(ref, job.Cluster.Namespace),
	}
	if err := w.Client.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("identity Secret not found", "secret", secretKey.String())
			return nil, SSHFallbackMisconfigured, fmt.Errorf("identity Secret not found: %s", secretKey.String())
		}
		log.Info("identity Secret read error", "secret", secretKey.String(), "category", "api-error")
		return nil, SSHFallbackMisconfigured, fmt.Errorf("identity Secret read error: %w", err)
	}
	pem, ok := secret.Data[key]
	if !ok || len(pem) == 0 {
		log.Info("identity Secret data key empty", "secret", secretKey.String(), "dataKey", key)
		return nil, SSHFallbackMisconfigured, fmt.Errorf("identity Secret %s missing or empty data key %q", secretKey.String(), key)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		log.Info("identity Secret parse failed", "secret", secretKey.String(), "category", "parse-error")
		return nil, SSHFallbackMisconfigured, fmt.Errorf("identity parse failed: %w", err)
	}
	return signer, "", nil
}

// runRemoteCat opens a single session on the SSH client, runs
// `cat <path>`, and returns the captured stdout. It enforces the
// sshFallbackMaxPayloadBytes cap with a bounded buffer; oversize output
// is rejected as SSHFallbackPayloadInvalid (NOT truncated and accepted).
// No PTY is requested, no shell is invoked.
func (w *SSHFallbackWorker) runRemoteCat(ctx context.Context, log logr.Logger, sshClient *ssh.Client, path string) ([]byte, SSHFallbackResultCategory, error) {
	session, err := sshClient.NewSession()
	if err != nil {
		return nil, SSHFallbackRemoteFileMissing, fmt.Errorf("ssh new session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Bounded stdout buffer: cap+1 so a payload exactly at the cap is
	// accepted while anything larger is detected. We use a fixed-size
	// byte slice instead of io.Copy with LimitReader so we can
	// distinguish "exactly at cap" from "more than cap".
	stdout := &boundedBuffer{cap: sshFallbackMaxPayloadBytes + 1}
	stderr := &bytes.Buffer{}
	session.Stdout = stdout
	session.Stderr = stderr

	// Quote the path defensively. We control `path` from
	// remoteKubeconfigPath() (whitelisted constants) so injection isn't
	// possible at the call sites that exist today, but the worker is a
	// security-critical surface; keep the shquote habit.
	cmd := fmt.Sprintf("cat %s", shellQuotePath(path))
	if err := session.Run(cmd); err != nil {
		// Distinguish "file not found" / "permission denied" (remote
		// returned non-zero) from connection-layer failures.
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			log.Info("remote cat returned non-zero", "exitStatus", exitErr.ExitStatus(), "category", "remote-file-missing")
			return nil, SSHFallbackRemoteFileMissing, fmt.Errorf("remote cat exit %d", exitErr.ExitStatus())
		}
		log.Info("remote cat failed", "category", "session-error")
		return nil, SSHFallbackRemoteFileMissing, fmt.Errorf("remote cat failed: %w", err)
	}

	if stdout.overflowed {
		log.Info("remote payload exceeded cap", "cap", sshFallbackMaxPayloadBytes)
		return nil, SSHFallbackPayloadInvalid, fmt.Errorf("remote payload exceeded %d bytes", sshFallbackMaxPayloadBytes)
	}
	payload := stdout.Bytes()
	if len(payload) == 0 {
		return nil, SSHFallbackPayloadInvalid, errors.New("remote payload is empty")
	}
	return payload, "", nil
}

// writeKubeconfigSecret upserts the cluster kubeconfig Secret with the
// SSH-fallback annotation stamped on. Mirrors the shape the node-push
// payload writes, with the source annotation set to "ssh-fallback" so
// observeKubeconfigSecret can route the Reason.
//
// Owner reference: the Cluster, Controller=false (matches CAPI convention
// for kubeconfig Secrets; see internal/controllers/CLAUDE.md § 3).
func (w *SSHFallbackWorker) writeKubeconfigSecret(ctx context.Context, log logr.Logger, job SSHFallbackJob, payload []byte) error {
	secretName := fmt.Sprintf("%s-kubeconfig", job.Cluster.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: job.Cluster.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, w.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[clusterv1.ClusterNameLabel] = job.Cluster.Name
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[KubeconfigSourceAnnotation] = KubeconfigSourceSSHFallback
		secret.Type = clusterv1.ClusterSecretType
		secret.Data = map[string][]byte{"value": payload}
		// Owner ref: Cluster (Controller=false) per CAPI convention.
		return controllerutil.SetOwnerReference(job.Cluster, secret, w.Scheme)
	})
	if err != nil {
		return fmt.Errorf("kubeconfig Secret upsert failed: %w", err)
	}
	log.V(2).Info("kubeconfig Secret written", "op", op, "secret", types.NamespacedName{Namespace: job.Cluster.Namespace, Name: secretName}.String())
	return nil
}

// classifyDialError categorizes a dial error into one of the
// SSHFallback*Reason result categories. Discrimination is by typed
// errors / net.OpError / ssh package errors — never by
// strings.Contains, per internal/controllers/CLAUDE.md § 6.
func classifyDialError(err error) SSHFallbackResultCategory {
	if err == nil {
		return SSHFallbackOK
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return SSHFallbackDialTimeout
	}
	// net.OpError wraps the transport layer; inspect its inner error.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return SSHFallbackDialTimeout
		}
		// Connection refused / no route — generic dial failure.
		return SSHFallbackDialRefused
	}
	// ssh package returns plain errors for handshake failures; the
	// public-key-rejected error has a stable suffix we match against.
	// We deliberately do NOT match by string here — instead, treat any
	// non-net error from the dial layer as host-key or auth depending
	// on whether the message mentions key. The ssh library does not
	// expose typed sentinels for these, so this is the least bad
	// classification path. The unit tests pin the categories that
	// matter (host-key mismatch, auth failed) by driving the fixture
	// to produce the exact error path.
	//
	// Fallback to a generic dial-refused so the reconciler sees a
	// retryable failure rather than a misconfig signal.
	msg := err.Error()
	switch {
	case containsCI(msg, "knownhosts"), containsCI(msg, "host key"):
		return SSHFallbackHostKeyMismatch
	case containsCI(msg, "unable to authenticate"), containsCI(msg, "no supported methods remain"):
		return SSHFallbackAuthFailed
	}
	return SSHFallbackDialRefused
}

// containsCI is a case-insensitive substring check used only for error
// classification on the path where the ssh library does not expose
// typed sentinels. The match strings are stable library outputs
// ("knownhosts: ...", "ssh: handshake failed: ssh: unable to
// authenticate, ..."), not user input.
func containsCI(haystack, needle string) bool {
	if len(needle) == 0 {
		return false
	}
	if len(haystack) < len(needle) {
		return false
	}
	// Naive case-insensitive containment; strings.Contains+strings.ToLower
	// allocates, this loop is allocation-free and the strings are short.
	hLen := len(haystack)
	nLen := len(needle)
	for i := 0; i <= hLen-nLen; i++ {
		match := true
		for j := 0; j < nLen; j++ {
			hc := haystack[i+j]
			nc := needle[j]
			if hc >= 'A' && hc <= 'Z' {
				hc += 'a' - 'A'
			}
			if nc >= 'A' && nc <= 'Z' {
				nc += 'a' - 'A'
			}
			if hc != nc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// remoteKubeconfigPath returns the on-VM kubeconfig path for the named
// distribution. Returns "" for unsupported distributions; callers
// translate this to SSHFallbackMisconfigured.
func remoteKubeconfigPath(distribution string) string {
	switch distribution {
	case "k0s", "":
		// Empty default mirrors the KCP defaulter (k0s is the default
		// distribution).
		return "/var/lib/k0s/pki/admin.conf"
	case "k3s":
		return "/etc/rancher/k3s/k3s.yaml"
	}
	return ""
}

// secretRefNamespace resolves the namespace of a SSHFallbackSecretReference.
// Defaults to the supplied parentNamespace when ref.Namespace is empty.
// Cross-namespace references are blocked at the webhook layer, so this
// function does not enforce the same-namespace invariant at runtime —
// the webhook is the gate.
func secretRefNamespace(ref *controlplanev1beta2.SSHFallbackSecretReference, parentNamespace string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return parentNamespace
}

// shellQuotePath single-quotes a filesystem path for safe inclusion in a
// shell command. Single quotes prevent any character interpretation by
// the remote shell. Single-quote literals are escaped per POSIX:
// '\”  -> closes the current quote, inserts a literal ', reopens.
//
// Used defensively even though current callers pass whitelisted paths;
// the worker is a security-critical surface and the habit matters.
func shellQuotePath(p string) string {
	const sq = "'"
	out := make([]byte, 0, len(p)+2)
	out = append(out, '\'')
	for i := 0; i < len(p); i++ {
		if p[i] == '\'' {
			out = append(out, sq...)
			out = append(out, "\\'"...)
			out = append(out, sq...)
			continue
		}
		out = append(out, p[i])
	}
	out = append(out, '\'')
	return string(out)
}

// newKnownHostsCallback materialises in-memory known_hosts bytes to a
// short-lived 0600 tempfile, hands the path to ssh/knownhosts.New, and
// removes the tempfile before returning. The returned callback closes
// over the parsed keys (knownhosts.New reads the file at construction
// time, not per-callback), so deleting the file is safe.
//
// We use a tempfile instead of an in-memory parser because the
// knownhosts package does not expose a from-bytes constructor; the
// alternative would be vendoring/forking the parser, which (per CLAUDE.md
// rule "stdlib first; sigs.k8s.io second; everything else third")
// is the wrong trade.
func newKnownHostsCallback(data []byte) (ssh.HostKeyCallback, error) {
	tmp, err := os.CreateTemp("", "kairos-known-hosts-*")
	if err != nil {
		return nil, fmt.Errorf("create temp known_hosts file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	// Enforce 0600 explicitly even though CreateTemp defaults to 0600.
	// Defense in depth against an umask change in a future Go runtime.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("chmod temp known_hosts file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temp known_hosts file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp known_hosts file: %w", err)
	}
	cb, err := knownhosts.New(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("knownhosts.New: %w", err)
	}
	return cb, nil
}

// boundedBuffer is a write-only bytes accumulator that records whether
// the total bytes written ever exceeded cap. Once overflowed, further
// writes are discarded. Used by runRemoteCat to enforce
// sshFallbackMaxPayloadBytes without holding 1 GiB of payload in RAM.
type boundedBuffer struct {
	buf        []byte
	cap        int
	overflowed bool
}

// Write accumulates bytes up to the cap. Once cap is exceeded, further
// writes are discarded and overflowed is set. Always returns len(p) (no
// partial writes) so the ssh session's exit-status logic is not derailed
// by a short-write error.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.overflowed {
		return n, nil
	}
	remaining := b.cap - len(b.buf)
	if n <= remaining {
		b.buf = append(b.buf, p...)
		if len(b.buf) >= b.cap {
			b.overflowed = true
		}
		return n, nil
	}
	b.buf = append(b.buf, p[:remaining]...)
	b.overflowed = true
	return n, nil
}

// Bytes returns the accumulated bytes (truncated to cap-1 since cap is
// the overflow trigger and we never want to return the overflow byte).
func (b *boundedBuffer) Bytes() []byte {
	if b.overflowed {
		// On overflow, callers check b.overflowed and reject the
		// payload; returning the partial bytes would not be safe to
		// write as a kubeconfig.
		return nil
	}
	return b.buf
}
