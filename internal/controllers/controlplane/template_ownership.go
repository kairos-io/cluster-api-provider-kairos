package controlplane

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// reconcileTemplateOwnerRefs makes the templates a KairosControlPlane references
// owned by the Cluster, mirroring KubeadmControlPlane.reconcileExternalReference.
//
// The reason is `clusterctl move`. Move carries an object only if it is linked to
// the Cluster through an OwnerReference chain, so without this the machine and
// config templates stay behind on the source management cluster while the
// KairosControlPlane arrives on the target still naming them. Observed on a real
// move between two management clusters: both templates were left behind and the
// moved control plane's infrastructureRef and kairosConfigTemplate pointed at
// objects that did not exist there, which fails the next clone with
// "failed to get infrastructure template: ... not found".
//
// The reference is deliberately NOT a controller reference. A template is shared
// by design — several control planes, or a MachineDeployment, can reference the
// same one — and a controller reference is exclusive, so claiming it would make
// the second referrer fail with "already owned by another controller". An
// ordinary owner reference also composes correctly for garbage collection:
// Kubernetes only collects an object once *every* owner is gone, so a template
// shared by two clusters survives the deletion of the first.
func (r *KairosControlPlaneReconciler) reconcileTemplateOwnerRefs(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	refs := []corev1.ObjectReference{kcp.Spec.MachineTemplate.InfrastructureRef}

	// The bootstrap template reference carries only a name in the common case;
	// fill in this provider's own group/version/kind so it resolves like any
	// other template.
	if name := kcp.Spec.KairosConfigTemplate.Name; name != "" {
		ref := corev1.ObjectReference{
			APIVersion: kcp.Spec.KairosConfigTemplate.APIVersion,
			Kind:       kcp.Spec.KairosConfigTemplate.Kind,
			Name:       name,
			Namespace:  kcp.Namespace,
		}
		if ref.APIVersion == "" {
			ref.APIVersion = bootstrapv1beta2.GroupVersion.String()
		}
		if ref.Kind == "" {
			ref.Kind = "KairosConfigTemplate"
		}
		refs = append(refs, ref)
	}

	for _, ref := range refs {
		if err := r.ensureTemplateOwnedByCluster(ctx, ref, kcp.Namespace, cluster); err != nil {
			return err
		}
	}
	return nil
}

// ensureTemplateOwnedByCluster adds the Cluster as an owner of one referenced
// template, if the reference names a template at all and the object exists.
func (r *KairosControlPlaneReconciler) ensureTemplateOwnedByCluster(ctx context.Context, ref corev1.ObjectReference, defaultNamespace string, cluster *clusterv1.Cluster) error {
	log := ctrllog.FromContext(ctx)

	// Only templates. The same field can name a concrete InfraMachine on some
	// paths, and that object is owned by its Machine, not by the Cluster.
	if ref.Name == "" || !strings.HasSuffix(ref.Kind, clusterv1.TemplateSuffix) {
		return nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		// Same defaulting the clone path uses: a reference without a namespace
		// resolves in the owner's namespace.
		namespace = defaultNamespace
	}
	// Never make the Cluster an owner of an object in another namespace.
	// Kubernetes treats an owner reference to a namespaced object in a different
	// namespace as absent, so the garbage collector would be free to delete the
	// template, which belongs to someone else. Such a reference is rejected at
	// admission and refused by the clone path; this is the last line.
	if namespace != cluster.Namespace {
		log.Info("Not taking ownership of a template in another namespace",
			"kind", ref.Kind, "name", ref.Name, "namespace", namespace, "cluster", cluster.Name)
		return nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ref.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, obj); err != nil {
		// Two ways a template can be absent, and neither is this helper's error
		// to report: the object does not exist (NotFound), or its CRD is not
		// installed at all, so the kind is not served and the REST mapper
		// refuses the request before it reaches the API server (NoKindMatch --
		// note this is *not* a NotFound). The latter is the ordinary state of a
		// cluster where the infrastructure provider has not been installed yet.
		// The clone path reports both with far better context, and failing here
		// would abort the whole reconcile and mask that message.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("get template %s %s/%s for owner reference: %w", ref.Kind, namespace, ref.Name, err)
	}

	desired := metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	}
	if util.HasExactOwnerRef(obj.GetOwnerReferences(), desired) {
		return nil
	}

	original := obj.DeepCopy()
	obj.SetOwnerReferences(util.EnsureOwnerRef(obj.GetOwnerReferences(), desired))
	if err := r.Patch(ctx, obj, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("set Cluster owner reference on %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	log.Info("Set Cluster owner reference on template so clusterctl move carries it",
		"kind", ref.Kind, "name", ref.Name, "namespace", namespace, "cluster", cluster.Name)
	return nil
}
