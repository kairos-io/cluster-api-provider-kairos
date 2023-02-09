package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ipfs/go-log/v2"
	infrastructurev1alpha3 "github.com/kairos-io/cluster-api-provider-kairos/api/v1alpha3"
	"github.com/mudler/edgevpn/pkg/config"
	"github.com/mudler/edgevpn/pkg/services"
	"github.com/mudler/go-nodepair"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mudler/edgevpn/pkg/logger"
	node "github.com/mudler/edgevpn/pkg/node"
)

func (r *KairosClusterReconciler) registerer(cluster infrastructurev1alpha3.KairosCluster, capiCluster *v1alpha3.Cluster) (c ctrl.Result, err error) {
	// switch phase := v1beta1.ClusterPhase(capiCluster.Status.Phase); phase {
	// 	case
	// 	v1beta1.ClusterPhasePending,
	// 	v1beta1.ClusterPhaseProvisioning,
	// 	v1beta1.ClusterPhaseProvisioned,
	// 	v1beta1.ClusterPhaseDeleting,
	// 	v1beta1.ClusterPhaseFailed:
	// 		return phase
	// 	default:
	// 		return ClusterPhaseUnknown
	// 	}

	switch cluster.Status.State {
	case &infrastructurev1alpha3.DeployingState:
		fmt.Println("Deploying", cluster.ClusterName)
		if cluster.Status.Nodes == cluster.Spec.Nodes {
			helper, err := patch.NewHelper(&cluster, r.Client)
			if err != nil {
				return ctrl.Result{}, err
			}
			cluster.Status.State = &infrastructurev1alpha3.ProvisioningState
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			if err := helper.Patch(ctx, &cluster); err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", cluster.Name)
			}
		}
	case &infrastructurev1alpha3.ProvisioningState:
		//  TODO: Check with edgevpn API the status, and get cluster kubeconfig and network token
		fmt.Println("Deploy completed. Provisioning machines.")
	case nil:
		return r.initialize(cluster, capiCluster)
	}

	//capiCluster.Spec.
	return
}

func (r *KairosClusterReconciler) initialize(cluster infrastructurev1alpha3.KairosCluster, capiCluster *v1alpha3.Cluster) (c ctrl.Result, err error) {

	// Generate a new bootstrap token if not present
	if cluster.Spec.BootstrapToken == "" {
		token := nodepair.GenerateToken()
		helper, err := patch.NewHelper(&cluster, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		cluster.Spec.BootstrapToken = token
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		if err := helper.Patch(ctx, &cluster); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", cluster.Name)
		}

		return ctrl.Result{}, errors.Wrapf(err, "re-enqueue %s", cluster.Name)
	}

	if cluster.Spec.NetworkToken == "" {
		token := nodepair.GenerateToken()
		helper, err := patch.NewHelper(&cluster, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		cluster.Spec.NetworkToken = token
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		if err := helper.Patch(ctx, &cluster); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", cluster.Name)
		}

		return ctrl.Result{}, errors.Wrapf(err, "re-enqueue %s", cluster.Name)
	}

	// XXX: Cloud config should be provided by the user as a secret ref already.
	// TODO: Get rid of CloudConfig in the kairoscluster type, use a secret ref
	secret, err := r.clientSet.CoreV1().Secrets(cluster.Namespace).Get(context.Background(), cluster.Name, v1.GetOptions{})
	if err != nil || secret == nil {
		_, err := r.clientSet.CoreV1().Secrets(cluster.Namespace).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
				Labels:    map[string]string{v1alpha3.ClusterLabelName: cluster.Name},
			},
			StringData: map[string]string{"value": cluster.Spec.CloudConfig},
		}, v1.CreateOptions{})
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "re-enqueue %s", cluster.Name)
		}
	}

	// TODO: Generate ISOs or other booting medium here if not present already

	r.queue <- nil

	// // patch from sigs.k8s.io/cluster-api/util/patch
	// helper, err := patch.NewHelper(&cluster, r.Client)
	// if err != nil {
	// 	return ctrl.Result{}, err
	// }
	// cluster.Status.Status = "foo"
	// if err := helper.Patch(ctx, &cluster); err != nil {
	// 	return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", cluster.Name)
	// }
	return ctrl.Result{}, nil
}

func registerNode(ctx context.Context, logLevel, token, device, cloudConfig string, options map[string]string) error {
	config := map[string]string{
		"device": device, // TODO: make optional
		"cc":     cloudConfig,
	}

	return nodepair.Send(
		ctx,
		config,
		nodepair.WithToken(token),
		nodepair.WithLogLevel(logLevel),
	)
}

func newNode(loglevel, token string) *node.Node {
	lvl, err := log.LevelFromString(loglevel)
	llger := logger.New(lvl)
	defaultInterval := 10 * time.Second

	if loglevel == "" {
		loglevel = "fatal"
	}

	c := config.Config{
		NetworkToken:   token,
		LowProfile:     true,
		LogLevel:       loglevel,
		Libp2pLogLevel: "fatal",
		Ledger: config.Ledger{
			SyncInterval:     defaultInterval,
			AnnounceInterval: defaultInterval,
		},
		NAT: config.NAT{
			Service:           true,
			Map:               true,
			RateLimit:         true,
			RateLimitGlobal:   10,
			RateLimitPeer:     10,
			RateLimitInterval: defaultInterval,
		},
		Discovery: config.Discovery{
			DHT:      true,
			MDNS:     true,
			Interval: 30 * time.Second,
		},
		Connection: config.Connection{
			HolePunch:      true,
			AutoRelay:      true,
			RelayV1:        true,
			MaxConnections: 10,
		},
	}

	nodeOpts, _, err := c.ToOpts(llger)
	if err != nil {
		return nil
	}

	nodeOpts = append(nodeOpts, services.Alive(30*time.Second, 900*time.Second, 15*time.Minute)...)

	n, err := node.New(nodeOpts...)
	if err != nil {
		return nil
	}
	return n
}

func (r *KairosClusterReconciler) registerNodes(ctx context.Context, timer time.Duration) {
	reSync := func() {
		fmt.Println("Registering new nodes")

		namespaceList, err := r.clientSet.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
		if err != nil {
			fmt.Println("Can't list namespaces", err)
			panic(err)
		}

		for _, i := range namespaceList.Items {
			clusterList := &infrastructurev1alpha3.KairosClusterList{}
			err = r.Client.List(ctx, clusterList, &client.ListOptions{Namespace: i.Name})
			for _, cluster := range clusterList.Items {
				if cluster.Spec.Nodes != cluster.Status.Nodes {
					fmt.Println("Registering new nodes from", cluster.Name)
					err := registerNode(ctx, "info", cluster.Spec.BootstrapToken, cluster.Spec.Device, cluster.Spec.CloudConfig, cluster.Spec.Options)
					if err != nil {
						fmt.Println("Failed registering node")
						return
					}
				}
			}
		}
	}

	tickerService(timer, ctx, r.queue, reSync)()
}

// GetKairosMachinesInCluster gets a cluster's KairosMachines resources.
func GetKairosMachinesInCluster(
	ctx context.Context,
	controllerClient client.Client,
	namespace, clusterName string) ([]*infrastructurev1alpha3.KairosMachine, error) {

	labels := map[string]string{v1alpha3.ClusterLabelName: clusterName}
	machineList := &infrastructurev1alpha3.KairosMachineList{}

	if err := controllerClient.List(
		ctx, machineList,
		client.InNamespace(namespace),
		client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	machines := make([]*infrastructurev1alpha3.KairosMachine, len(machineList.Items))
	for i := range machineList.Items {
		machines[i] = &machineList.Items[i]
	}

	return machines, nil
}

func (r *KairosClusterReconciler) watchNodes(ctx context.Context, timer time.Duration) {
	reSync := func() {
		fmt.Println("Watching new nodes")
		namespaceList, err := r.clientSet.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
		if err != nil {
			fmt.Println("Can't list namespaces", err)
			panic(err)
		}

		for _, i := range namespaceList.Items {
			clusterList := &infrastructurev1alpha3.KairosClusterList{}
			err = r.Client.List(ctx, clusterList, &client.ListOptions{Namespace: i.Name})

			for _, cluster := range clusterList.Items {

				if cluster.Spec.Nodes != cluster.Status.Nodes {
					fmt.Println("Watching new nodes for", cluster.Name)

					// TODO: Create MachineNodes instead, so we can compare if we have them already
					// also - use a separate field. the nodes we retrieve here
					// are nodes that are in the provisioning stage, but no kubernetes has been formed yet.
					// We need to inject a payload that starts a tunnel which we can use to connect back and issue
					// kubectl commands at. At that point we can send over plans to update the cluster, check the state
					// and gather all the node infos.
					n := returnNodes(cluster.Spec.BootstrapToken)
					copy := cluster.DeepCopy()
					copy.Status.Nodes = len(n)
					fmt.Println("Found ", n, "Nodes ready, updating")
					err := r.Client.Status().Update(ctx, copy)
					if err != nil {
						fmt.Println("Failed updating cluster", cluster.Name, err)
					}
					for _, m := range n {
						machine := &infrastructurev1alpha3.KairosMachine{
							ObjectMeta: metav1.ObjectMeta{
								Name:      strings.ToLower(m),
								Namespace: cluster.Namespace,

								Labels: map[string]string{v1alpha3.ClusterLabelName: cluster.Name},
							},
							Spec: infrastructurev1alpha3.KairosMachineSpec{
								UUID: m,
							},
						}
						machineFound := &infrastructurev1alpha3.KairosMachine{}
						err := r.Client.Get(ctx, client.ObjectKeyFromObject(machine), machineFound)
						if err != nil {
							err = r.Client.Create(ctx, machine)
							if err != nil {
								fmt.Println("Failed updating cluster", cluster.Name, err)
							}
						}

						err = r.Client.Get(ctx, client.ObjectKeyFromObject(machine), machineFound)
						if err != nil {

							fmt.Println("Failed updating cluster", cluster.Name, err)

						}

						provider := "kairos"
						capiMachine := &v1alpha3.Machine{
							ObjectMeta: metav1.ObjectMeta{
								Name:      strings.ToLower(m),
								Namespace: cluster.Namespace,
								Labels:    map[string]string{v1alpha3.ClusterLabelName: cluster.Name},
							},
							Spec: v1alpha3.MachineSpec{
								ProviderID:  &provider,
								ClusterName: cluster.Name,
								// TODO: Link data cloudconfig here with CRDs cloudconfig
								// during object creation
								Bootstrap: v1alpha3.Bootstrap{
									DataSecretName: &cluster.Name,
								},
								InfrastructureRef: corev1.ObjectReference{
									Kind:      "KairosMachine",
									Namespace: machine.ObjectMeta.Namespace,
									Name:      machine.ObjectMeta.Name,
									UID:       machineFound.ObjectMeta.UID,
								},
							},
						}
						capiMachineFound := &v1alpha3.Machine{}
						err = r.Client.Get(ctx, client.ObjectKeyFromObject(capiMachine), capiMachineFound)
						if err != nil {
							err = r.Client.Create(ctx, capiMachine)
							if err != nil {
								fmt.Println("Failed updating cluster", cluster.Name, err)
							}
						}
					}
				}
			}
		}
	}

	tickerService(timer, ctx, r.queue, reSync)()
}

func tickerService(dur time.Duration, ctx context.Context, c chan interface{}, fn func()) func() {
	return func() {
		ticker := time.NewTicker(dur)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fn()
			case <-c:
				fn()
			}
		}
	}
}

func returnNodes(token string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	n := newNode("info", token)
	n.Start(ctx)

	l, err := n.Ledger()
	if err != nil {
		return []string{}
	}
	uuids := []string{}

	for {
		select {
		case <-ctx.Done():
			return uuids
		default:
			nn := l.CurrentData()["pairing"]
			fmt.Println(l.CurrentData())
			uuids = []string{}
			for a, _ := range nn {
				if "data" == a {
					continue
				}
				uuids = append(uuids, a)
			}
			if len(uuids) > 0 {
				return uuids
			}
			time.Sleep(1 * time.Second)
		}
	}
}
