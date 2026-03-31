package kubevirtenv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlserializer "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// ApplyManifestFromURL downloads YAML and applies it with server-side apply.
func (e *Environment) ApplyManifestFromURL(dynamicClient dynamic.Interface, config *rest.Config, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download manifest: HTTP %d", resp.StatusCode)
	}
	yamlContent, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	return e.ApplyManifestContent(dynamicClient, config, yamlContent)
}

// ApplyManifestContent applies multi-document YAML with server-side apply.
func (e *Environment) ApplyManifestContent(dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	gr, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return fmt.Errorf("API group resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(gr)
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(yamlContent)), 4096)
	dec := yamlserializer.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	for {
		var rawObj runtime.RawExtension
		if err := decoder.Decode(&rawObj); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if len(rawObj.Raw) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		_, gvk, err := dec.Decode(rawObj.Raw, nil, obj)
		if err != nil {
			log.Warnf("decode resource: %v", err)
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			log.Warnf("REST mapping for %s: %v", gvk, err)
			continue
		}
		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == "namespace" && obj.GetNamespace() != "" {
			dr = dynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			dr = dynamicClient.Resource(mapping.Resource)
		}
		obj.SetManagedFields(nil)
		_, err = dr.Apply(context.Background(), obj.GetName(), obj, metav1.ApplyOptions{
			FieldManager: applyFieldManager,
		})
		if err != nil {
			_, createErr := dr.Create(context.Background(), obj, metav1.CreateOptions{})
			if createErr != nil && !strings.Contains(createErr.Error(), "already exists") {
				log.Warnf("apply %s/%s: %v", gvk.Kind, obj.GetName(), err)
			}
		}
	}
	return nil
}

// ApplyManifestFromFile reads a YAML file and applies it.
func (e *Environment) ApplyManifestFromFile(dynamicClient dynamic.Interface, config *rest.Config, filePath string) error {
	yamlContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read manifest file: %w", err)
	}
	return e.ApplyManifestContent(dynamicClient, config, yamlContent)
}

// DeleteResourcesFromManifestFile deletes resources described in a local YAML file.
func (e *Environment) DeleteResourcesFromManifestFile(dynamicClient dynamic.Interface, config *rest.Config, filePath string) error {
	yamlContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read manifest file: %w", err)
	}
	return e.deleteResourcesFromYAML(dynamicClient, config, yamlContent)
}

// DeleteResourcesFromManifestURL deletes resources described in a remote YAML manifest.
func (e *Environment) DeleteResourcesFromManifestURL(dynamicClient dynamic.Interface, config *rest.Config, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download manifest: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	return e.deleteResourcesFromYAML(dynamicClient, config, body)
}

func (e *Environment) deleteResourcesFromYAML(dynamicClient dynamic.Interface, config *rest.Config, yamlContent []byte) error {
	log := e.log()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	gr, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return fmt.Errorf("API group resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(gr)
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(yamlContent)), 4096)
	dec := yamlserializer.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	for {
		var rawObj runtime.RawExtension
		if err := decoder.Decode(&rawObj); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if len(rawObj.Raw) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		_, gvk, err := dec.Decode(rawObj.Raw, nil, obj)
		if err != nil {
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}
		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == "namespace" && obj.GetNamespace() != "" {
			dr = dynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			dr = dynamicClient.Resource(mapping.Resource)
		}
		err = dr.Delete(context.Background(), obj.GetName(), metav1.DeleteOptions{})
		if err != nil && !strings.Contains(err.Error(), "not found") {
			log.Warnf("delete %s/%s: %v", gvk.Kind, obj.GetName(), err)
		}
	}
	return nil
}
