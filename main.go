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

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

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

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
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
		LeaderElectionID:       "kairos-capi-leader-election",
	}

	// Set cache namespace if WATCH_NAMESPACE is configured
	if !cfg.ShouldWatchAllNamespaces() {
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				cfg.GetWatchNamespace(): {},
			},
		}
		setupLog.Info("Watching single namespace", "namespace", cfg.GetWatchNamespace())
	} else {
		setupLog.Info("Watching all namespaces")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

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
	if err = (&bootstrap.KairosConfigReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		MgmtEndpointResolver: mgmtResolver,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KairosConfig")
		os.Exit(1)
	}

	if err = (&controlplane.KairosControlPlaneReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("kairoscontrolplane-controller"),
		// WorkloadClientFactory left nil → defaultWorkloadClient (builds a client
		// from the <cluster>-kubeconfig Secret) for the etcd-leave handshake.
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KairosControlPlane")
		os.Exit(1)
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
	if err = sshFallbackReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KairosControlPlane.SSHFallback")
		os.Exit(1)
	}
	if err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return sshFallbackReconciler.StartResultDrain(ctx)
	})); err != nil {
		setupLog.Error(err, "unable to add SSHFallback result drain runnable")
		os.Exit(1)
	}

	if err = (&bootstrapv1beta2.KairosConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "KairosConfig")
		os.Exit(1)
	}
	if err = (&controlplanev1beta2.KairosControlPlane{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "KairosControlPlane")
		os.Exit(1)
	}
	if err = (&controlplanev1beta2.KairosControlPlaneTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "KairosControlPlaneTemplate")
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
