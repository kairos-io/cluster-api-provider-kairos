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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/config"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(bootstrapv1beta2.AddToScheme(scheme))
	utilruntime.Must(controlplanev1beta2.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// controllerRole selects which set of controllers and webhooks a single
// manager process registers. ADR 0007 (Option A1) packages this one container
// image as two clusterctl providers by running it twice with different roles:
// a bootstrap Deployment (--controllers=bootstrap) and a control-plane
// Deployment (--controllers=control-plane). The historical flat, kubectl-apply
// manifest keeps running --controllers=all (the default), which registers both
// providers in a single process exactly as before.
type controllerRole int

const (
	// roleAll registers both provider types in one process. This is the
	// default and the flat-manifest behavior; it must stay byte-for-byte
	// equivalent to the pre-ADR-0007 wiring.
	roleAll controllerRole = iota
	// roleBootstrap registers only the bootstrap controllers + webhook.
	roleBootstrap
	// roleControlPlane registers only the control-plane controllers + webhooks.
	roleControlPlane
)

// parseControllerRole maps a raw --controllers flag value to a controllerRole.
//
// The match is exact: case-sensitive with no surrounding-whitespace trimming.
// This flag is a machine-set knob baked into the provider Deployment overlays
// (ADR 0007), not free-form operator input, so a strict match surfaces a typo
// in the manifest loudly (startup failure) instead of silently degrading to a
// partial registration that would look like a mysteriously dead provider.
func parseControllerRole(v string) (controllerRole, error) {
	switch v {
	case "all":
		return roleAll, nil
	case "bootstrap":
		return roleBootstrap, nil
	case "control-plane":
		return roleControlPlane, nil
	default:
		return roleAll, fmt.Errorf(
			"invalid --controllers value %q: must be one of %q, %q, or %q",
			v, "bootstrap", "control-plane", "all")
	}
}

// enablesBootstrap reports whether this role registers the bootstrap
// controllers (KairosConfig reconciler + its mgmt-endpoint resolver wiring)
// and the KairosConfig admission webhook.
func (r controllerRole) enablesBootstrap() bool {
	return r == roleAll || r == roleBootstrap
}

// enablesControlPlane reports whether this role registers the control-plane
// controllers (KairosControlPlane reconciler + the PR-9 SSH-fallback sibling)
// and the KairosControlPlane / KairosControlPlaneTemplate webhooks.
func (r controllerRole) enablesControlPlane() bool {
	return r == roleAll || r == roleControlPlane
}

// leaderElectionID returns the lease name for this role. The bootstrap and
// control-plane Deployments (ADR 0007) run as two separate pods and MUST NOT
// share a lease — a shared lease would let one provider's leader perpetually
// block the other from ever becoming active. roleAll deliberately keeps the
// historical "kairos-capi-leader-election" ID UNCHANGED so existing
// flat-manifest installs retain their lease across an in-place upgrade.
func (r controllerRole) leaderElectionID() string {
	switch r {
	case roleBootstrap:
		return "kairos-bootstrap-leader-election"
	case roleControlPlane:
		return "kairos-control-plane-leader-election"
	default:
		return "kairos-capi-leader-election"
	}
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var controllersFlag string
	var namespaceFlag string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	// --controllers gates which provider role this manager process serves.
	// ADR 0007 (Option A1) runs this single image twice: once as the bootstrap
	// clusterctl provider and once as the control-plane provider. "all" (the
	// default) preserves the flat kubectl-apply manifest that registers both
	// providers in one process.
	flag.StringVar(&controllersFlag, "controllers", "all",
		"Which controllers/webhooks to register: \"bootstrap\", \"control-plane\", or \"all\".")
	// --namespace scopes the manager cache to a single namespace for clusterctl
	// single-namespace-watch compliance (the container contract requires this
	// flag). When set it takes precedence over the legacy WATCH_NAMESPACE env
	// var; when empty the existing WATCH_NAMESPACE behavior is preserved.
	flag.StringVar(&namespaceFlag, "namespace", "",
		"If set, restrict the manager to watch only this namespace "+
			"(takes precedence over the WATCH_NAMESPACE env var).")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Load configuration
	cfg := config.LoadConfig()

	// Set log level if specified
	if cfg.LogLevel == "debug" {
		opts.Development = true
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Resolve the provider role from --controllers before building the
	// manager: the role both selects the LeaderElectionID (two separate
	// Deployments must not share a lease) and gates which controllers/webhooks
	// register below. An invalid value is a hard startup error — fail loudly
	// rather than silently bringing up a partial provider.
	role, err := parseControllerRole(controllersFlag)
	if err != nil {
		setupLog.Error(err, "invalid --controllers flag")
		os.Exit(1)
	}

	// Configure manager options
	mgrOptions := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		// Role-specific so the bootstrap and control-plane Deployments (ADR
		// 0007) never contend for a shared lease; roleAll keeps the historical
		// ID so flat-manifest installs retain their lease across upgrades.
		LeaderElectionID: role.leaderElectionID(),
	}

	// --namespace (clusterctl single-namespace watch) takes precedence over the
	// legacy WATCH_NAMESPACE env path. When --namespace is empty the prior
	// WATCH_NAMESPACE behavior is preserved byte-for-byte (roleAll + empty
	// --namespace is the unchanged flat-manifest path).
	watchNamespace, watchSingle := "", false
	switch {
	case namespaceFlag != "":
		watchNamespace, watchSingle = namespaceFlag, true
	case !cfg.ShouldWatchAllNamespaces():
		watchNamespace, watchSingle = cfg.GetWatchNamespace(), true
	}
	if watchSingle {
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: {},
			},
		}
		setupLog.Info("Watching single namespace", "namespace", watchNamespace)
	} else {
		setupLog.Info("Watching all namespaces")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register controllers and webhooks gated by the resolved role. The two
	// helpers log the specific controller/webhook that failed (preserving the
	// prior structured log lines) and return a non-nil error, which we turn
	// into a non-zero exit here.
	setupLog.Info("registering controllers", "role", controllersFlag)
	if err := registerControllers(mgr, role); err != nil {
		os.Exit(1)
	}
	if err := registerWebhooks(mgr, role); err != nil {
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// registerControllers wires the reconcilers this manager's role owns. It is
// split out of main() so the role gating has one obvious home and a future
// envtest can drive it with a fake manager. Bootstrap and control-plane are
// registered independently so a role-scoped Deployment (ADR 0007) only runs
// the controllers it is responsible for. On failure it logs the specific
// controller that failed and returns a non-nil error for the caller to
// translate into a non-zero exit.
func registerControllers(mgr manager.Manager, role controllerRole) error {
	if role.enablesBootstrap() {
		// Wire the production management-endpoint resolver. Pulling the API
		// server URL from mgr.GetConfig().Host preserves the pre-KD-33 behavior
		// (the legacy ensureKubeconfigPushConfig used the same source). The
		// resolver itself short-circuits to (nil, nil) when ManagementAPIServer
		// is empty — the documented disabled signal — so an empty Host stays a
		// graceful "render without push block" rather than a startup error.
		//
		// KAIROS_MANAGEMENT_API_OVERRIDE: when set, overrides mgr.GetConfig().Host
		// as the URL nodes dial back to. Required for non-CAPK infrastructure
		// (CAPV, CAPM3, Tinkerbell) where workload VMs live on a different network
		// than the management cluster's service IP — the in-cluster service URL
		// (typically https://10.96.0.1:443 or kubernetes.default.svc) is not
		// routable from workload VMs on a LAN. Set this env var to a
		// LAN-reachable URL (e.g., https://<mgmt-cp-node-ip>:6443) on the
		// controller Deployment. PR-9's Spec.SSHFallback is the air-gapped
		// alternative for environments where no such reachability exists.
		//
		// This whole block is gated on the bootstrap role: only the bootstrap
		// reconciler consumes the resolver, so a control-plane-only manager has
		// no reason to read the management API URL or the override env var.
		mgmtAPIServer := mgr.GetConfig().Host
		if override := os.Getenv("KAIROS_MANAGEMENT_API_OVERRIDE"); override != "" {
			setupLog.Info("Overriding management API server URL from environment",
				"original", mgmtAPIServer, "override", override)
			mgmtAPIServer = override
		}
		mgmtResolver := bootstrap.NewKubeVirtTokenResolver(
			mgr.GetClient(),
			mgr.GetScheme(),
			mgmtAPIServer,
		)
		if err := (&bootstrap.KairosConfigReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			MgmtEndpointResolver: mgmtResolver,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "KairosConfig")
			return err
		}
	}

	if role.enablesControlPlane() {
		if err := (&controlplane.KairosControlPlaneReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("kairoscontrolplane-controller"),
			// WorkloadClientFactory left nil → defaultWorkloadClient (builds a client
			// from the <cluster>-kubeconfig Secret) for the etcd-leave handshake.
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "KairosControlPlane")
			return err
		}

		// Wire the PR-9 SSH-fallback sibling controller. The worker pool is a
		// process-singleton owned by main.go and shared with the reconciler:
		// the reconciler enqueues work, the worker performs the SSH dial in
		// a goroutine pool (bounded at 4 — see ssh_fallback_worker.go), and
		// the reconciler drains the result channel in a manager-managed
		// runnable so graceful shutdown is honoured.
		//
		// SECURITY: the worker enforces strict host-key verification via
		// golang.org/x/crypto/ssh/knownhosts.New; there is no TOFU path
		// anywhere in this wiring. The pool size is hard-coded (no flag) per
		// ADR 0002 § F.2.
		sshFallbackWorker := controlplane.NewSSHFallbackWorker(
			mgr.GetClient(),
			mgr.GetScheme(),
			mgr.GetEventRecorderFor("kairoscontrolplane-ssh-fallback"),
		)
		sshFallbackReconciler := &controlplane.SSHFallbackReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Worker: sshFallbackWorker,
		}
		if err := sshFallbackReconciler.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "KairosControlPlane.SSHFallback")
			return err
		}
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			return sshFallbackReconciler.StartResultDrain(ctx)
		})); err != nil {
			setupLog.Error(err, "unable to add SSHFallback result drain runnable")
			return err
		}
	}

	return nil
}

// registerWebhooks wires the admission webhooks this manager's role owns, gated
// identically to registerControllers so each role-scoped Deployment (ADR 0007)
// only serves the webhooks it is responsible for.
func registerWebhooks(mgr manager.Manager, role controllerRole) error {
	if role.enablesBootstrap() {
		if err := (&bootstrapv1beta2.KairosConfig{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "KairosConfig")
			return err
		}
	}
	if role.enablesControlPlane() {
		if err := (&controlplanev1beta2.KairosControlPlane{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "KairosControlPlane")
			return err
		}
		if err := (&controlplanev1beta2.KairosControlPlaneTemplate{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "KairosControlPlaneTemplate")
			return err
		}
	}
	return nil
}
