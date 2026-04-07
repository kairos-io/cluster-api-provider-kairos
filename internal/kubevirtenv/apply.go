package kubevirtenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlserializer "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
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
func (e *Environment) ApplyManifestContent(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	return forEachManifestObject(log, config, yamlContent, func(mapping *manifestMapping, obj *unstructured.Unstructured) error {
		dr := resourceClient(dynamicClient, mapping, obj)
		obj.SetManagedFields(nil)
		if _, err := dr.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: applyFieldManager}); err != nil {
			log.Warnf("apply %s/%s: %v", mapping.gvk.Kind, obj.GetName(), err)
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

func (e *Environment) deleteResourcesFromYAML(ctx context.Context, dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	return forEachManifestObject(log, config, yamlContent, func(mapping *manifestMapping, obj *unstructured.Unstructured) error {
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

// forEachManifestObject decodes a multi-doc YAML stream and invokes fn for each object that
// resolves through discovery. Decode/mapping errors are logged via the environment logger and skipped.
func forEachManifestObject(log Logger, config *rest.Config, yamlContent []byte, fn func(*manifestMapping, *unstructured.Unstructured) error) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	gr, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return fmt.Errorf("API group resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(gr)
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
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			if log != nil {
				log.Warnf("skip %s/%s: REST mapping unavailable: %v", gvk.Kind, obj.GetName(), err)
			}
			continue
		}
		mm := &manifestMapping{gvk: *gvk, mapping: mapping}
		if err := fn(mm, obj); err != nil {
			return err
		}
	}
	return nil
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
