package kubevirtenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlserializer "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// restMappingRetry* bound how long we wait for a just-installed CRD to be
// served by the API server's discovery endpoint. CRDs go through Established
// before kube-apiserver serves their kind; without retry we race the CRD and
// silently skip the dependent CR.
const (
	restMappingRetryTimeout  = 30 * time.Second
	restMappingRetryInterval = 1 * time.Second
)

// ApplyManifestFromURL downloads YAML and applies it with server-side apply.
func (e *Environment) ApplyManifestFromURL(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, url string) error {
	body, err := httpGetBody(ctx, url)
	if err != nil {
		return err
	}
	return e.ApplyManifestContent(ctx, dynamicClient, config, body)
}

// ApplyManifestContent applies multi-document YAML with server-side apply.
//
// Apply errors are returned to the caller. Previously they were downgraded to
// log.Warnf and the function returned nil — which silently masked CRD-ordering
// bugs (e.g. applying a CR before its CRD was Established) and made install
// failures look like flaky waits downstream.
//
// ApplyOptions.Force is set so we resolve field-manager conflicts in our
// favor: this is an installer tool whose job is to enforce a known-good state
// of the manifests. The typical conflict surface is kind-bundled resources
// (e.g. local-path-provisioner) that ship with `kubectl-client-side-apply` as
// their field manager. SSA documents Force as the standard mechanism for
// adopting fields owned by another manager.
func (e *Environment) ApplyManifestContent(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	return forEachManifestObject(ctx, log, config, yamlContent, func(mapping *manifestMapping, obj *unstructured.Unstructured) error {
		dr := resourceClient(dynamicClient, mapping, obj)
		obj.SetManagedFields(nil)
		opts := metav1.ApplyOptions{FieldManager: applyFieldManager, Force: true}
		if _, err := dr.Apply(ctx, obj.GetName(), obj, opts); err != nil {
			return fmt.Errorf("apply %s/%s: %w", mapping.gvk.Kind, obj.GetName(), err)
		}
		return nil
	})
}

// ApplyManifestFromFile reads a YAML file and applies it.
func (e *Environment) ApplyManifestFromFile(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, filePath string) error {
	yamlContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read manifest file: %w", err)
	}
	return e.ApplyManifestContent(ctx, dynamicClient, config, yamlContent)
}

// DeleteResourcesFromManifestFile deletes resources described in a local YAML file.
func (e *Environment) DeleteResourcesFromManifestFile(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, filePath string) error {
	yamlContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read manifest file: %w", err)
	}
	return e.deleteResourcesFromYAML(ctx, dynamicClient, config, yamlContent)
}

// DeleteResourcesFromManifestURL deletes resources described in a remote YAML manifest.
func (e *Environment) DeleteResourcesFromManifestURL(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, url string) error {
	body, err := httpGetBody(ctx, url)
	if err != nil {
		return err
	}
	return e.deleteResourcesFromYAML(ctx, dynamicClient, config, body)
}

// deleteResourcesFromYAML issues Delete for each object in the manifest stream.
// Per-object errors other than NotFound are downgraded to warnings: callers
// (e.g. UninstallKubeVirt) want best-effort cleanup and should not abort on
// the first stale object.
func (e *Environment) deleteResourcesFromYAML(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	return forEachManifestObject(ctx, log, config, yamlContent, func(mapping *manifestMapping, obj *unstructured.Unstructured) error {
		dr := resourceClient(dynamicClient, mapping, obj)
		if err := dr.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Warnf("delete %s/%s: %v", mapping.gvk.Kind, obj.GetName(), err)
		}
		return nil
	})
}

// manifestMapping pairs a parsed object's GVK with its REST mapping.
type manifestMapping struct {
	gvk     schema.GroupVersionKind
	mapping *meta.RESTMapping
}

// forEachManifestObject decodes a multi-doc YAML stream and invokes fn for
// each object. Decode failures abort. REST-mapping NoMatch errors trigger a
// bounded retry against a refreshed discovery cache, since a CRD applied
// earlier in the same operation may not be served by kube-apiserver yet.
func forEachManifestObject(ctx context.Context, log Logger, config *rest.Config, yamlContent []byte, fn func(*manifestMapping, *unstructured.Unstructured) error) error {
	mapper, err := buildRESTMapper(config)
	if err != nil {
		return err
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(yamlContent), 4096)
	dec := yamlserializer.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	for {
		var rawObj runtime.RawExtension
		if err := decoder.Decode(&rawObj); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode YAML document: %w", err)
		}
		if len(rawObj.Raw) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		_, gvk, err := dec.Decode(rawObj.Raw, nil, obj)
		if err != nil {
			return fmt.Errorf("decode object: %w", err)
		}
		mapping, err := resolveMapping(ctx, log, config, mapper, *gvk)
		if err != nil {
			return fmt.Errorf("REST mapping for %s/%s: %w", gvk.Kind, obj.GetName(), err)
		}
		mm := &manifestMapping{gvk: *gvk, mapping: mapping}
		if err := fn(mm, obj); err != nil {
			return err
		}
	}
	return nil
}

// resolveMapping returns the REST mapping for gvk, refreshing discovery if the
// initial lookup reports NoMatch (typical right after a CRD apply).
func resolveMapping(ctx context.Context, log Logger, config *rest.Config, mapper meta.RESTMapper, gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return mapping, nil
	}
	if !meta.IsNoMatchError(err) {
		return nil, err
	}
	if log != nil {
		log.Infof("REST mapping for %s/%s not yet served by API server; refreshing discovery (up to %s)", gvk.GroupKind().String(), gvk.Version, restMappingRetryTimeout)
	}
	waitErr := wait.PollUntilContextTimeout(ctx, restMappingRetryInterval, restMappingRetryTimeout, false, func(ctx context.Context) (bool, error) {
		rm, berr := buildRESTMapper(config)
		if berr != nil {
			return false, berr
		}
		m, mErr := rm.RESTMapping(gvk.GroupKind(), gvk.Version)
		if mErr == nil {
			mapping = m
			return true, nil
		}
		if !meta.IsNoMatchError(mErr) {
			return false, mErr
		}
		return false, nil
	})
	if waitErr != nil {
		return nil, waitErr
	}
	return mapping, nil
}

// buildRESTMapper returns a fresh discovery-backed RESTMapper. Callers rebuild
// after operations that may have changed the set of registered CRDs.
func buildRESTMapper(config *rest.Config) (meta.RESTMapper, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	gr, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("API group resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(gr), nil
}

func resourceClient(dynamicClient dynamic.Interface, mm *manifestMapping, obj *unstructured.Unstructured) dynamic.ResourceInterface {
	if mm.mapping.Scope.Name() == "namespace" && obj.GetNamespace() != "" {
		return dynamicClient.Resource(mm.mapping.Resource).Namespace(obj.GetNamespace())
	}
	return dynamicClient.Resource(mm.mapping.Resource)
}

// httpGetBody downloads url with the given context and returns the body bytes.
func httpGetBody(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download manifest: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return body, nil
}
