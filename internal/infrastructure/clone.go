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

package infrastructure

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// CloneInfrastructureMachine clones an infrastructure machine template into a new machine resource
func CloneInfrastructureMachine(ctx context.Context, c client.Client, scheme *runtime.Scheme, templateRef corev1.ObjectReference, machineName, namespace string, labels, annotations map[string]string) (client.Object, error) {
	logger := log.FromContext(ctx)

	// Log the template reference for debugging
	logger.Info("Cloning infrastructure machine",
		"kind", templateRef.Kind,
		"group", templateRef.GroupVersionKind().Group,
		"version", templateRef.GroupVersionKind().Version,
		"name", templateRef.Name,
		"namespace", templateRef.Namespace)

	// Get the template object
	templateObj, err := getTemplateObject(ctx, c, templateRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get infrastructure template: %w", err)
	}

	// Clone based on infrastructure provider type
	// Normalize the Kind to handle potential case variations
	kind := templateRef.Kind
	logger.Info("Processing infrastructure template", "kind", kind)

	switch kind {
	case "DockerMachineTemplate":
		return cloneDockerMachineTemplate(ctx, c, scheme, templateObj, machineName, namespace, labels, annotations)
	case "VSphereMachineTemplate":
		return cloneVSphereMachineTemplate(ctx, c, scheme, templateObj, machineName, namespace, labels, annotations)
	case "KubevirtMachineTemplate", "KubeVirtMachineTemplate":
		logger.Info("Cloning KubevirtMachineTemplate", "machineName", machineName)
		return cloneKubevirtMachineTemplate(ctx, c, scheme, templateObj, machineName, namespace, labels, annotations)
	default:
		return nil, fmt.Errorf("unsupported infrastructure provider: %s (Group: %s, Version: %s, FullGVK: %s)",
			kind,
			templateRef.GroupVersionKind().Group,
			templateRef.GroupVersionKind().Version,
			templateRef.GroupVersionKind().String())
	}
}

func getTemplateObject(ctx context.Context, c client.Client, ref corev1.ObjectReference) (*unstructured.Unstructured, error) {
	// Get the full template object as unstructured
	fullObj := &unstructured.Unstructured{}
	fullObj.SetGroupVersionKind(ref.GroupVersionKind())

	key := client.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}

	if err := c.Get(ctx, key, fullObj); err != nil {
		return nil, err
	}

	return fullObj, nil
}

func cloneDockerMachineTemplate(ctx context.Context, c client.Client, scheme *runtime.Scheme, template *unstructured.Unstructured, machineName, namespace string, labels, annotations map[string]string) (client.Object, error) {
	// For CAPD, we create a DockerMachine from DockerMachineTemplate
	// This is a simplified version - in production, you'd use the actual CAPD types

	// Create unstructured DockerMachine
	dockerMachine := &unstructured.Unstructured{}
	dockerMachine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachine",
	})

	dockerMachine.SetName(machineName)
	dockerMachine.SetNamespace(namespace)
	dockerMachine.SetLabels(labels)
	dockerMachine.SetAnnotations(annotations)

	// Copy spec from template
	if spec, ok, _ := unstructured.NestedMap(template.UnstructuredContent(), "spec", "template", "spec"); ok {
		if err := unstructured.SetNestedMap(dockerMachine.UnstructuredContent(), spec, "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}
	}

	return dockerMachine, nil
}

func cloneVSphereMachineTemplate(ctx context.Context, c client.Client, scheme *runtime.Scheme, template *unstructured.Unstructured, machineName, namespace string, labels, annotations map[string]string) (client.Object, error) {
	// For CAPV, we create a VSphereMachine from VSphereMachineTemplate
	// This is a simplified version - in production, you'd use the actual CAPV types

	vsphereMachine := &unstructured.Unstructured{}
	vsphereMachine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "VSphereMachine",
	})

	vsphereMachine.SetName(machineName)
	vsphereMachine.SetNamespace(namespace)
	vsphereMachine.SetLabels(labels)
	vsphereMachine.SetAnnotations(annotations)

	// Copy spec from template
	if spec, ok, _ := unstructured.NestedMap(template.UnstructuredContent(), "spec", "template", "spec"); ok {
		if err := unstructured.SetNestedMap(vsphereMachine.UnstructuredContent(), spec, "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}
	}

	return vsphereMachine, nil
}

func cloneKubevirtMachineTemplate(ctx context.Context, c client.Client, scheme *runtime.Scheme, template *unstructured.Unstructured, machineName, namespace string, labels, annotations map[string]string) (client.Object, error) {
	// For CAPK, we create a KubevirtMachine from KubevirtMachineTemplate

	kubevirtMachine := &unstructured.Unstructured{}
	version := template.GroupVersionKind().Version
	if version == "" {
		version = "v1alpha1"
	}
	kubevirtMachine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: version,
		Kind:    "KubevirtMachine",
	})

	kubevirtMachine.SetName(machineName)
	kubevirtMachine.SetNamespace(namespace)
	kubevirtMachine.SetLabels(labels)
	kubevirtMachine.SetAnnotations(annotations)

	// Copy virtualMachineBootstrapCheck from template (e.g. checkStrategy: none to bypass SSH sentinel check)
	// Template: spec.template.spec.virtualMachineBootstrapCheck -> KubevirtMachine: spec.virtualMachineBootstrapCheck
	// Default to "none" when not set - kubeconfig push avoids SSH; SSH often fails in bridged/Virtio setups
	bootstrapCheck := map[string]interface{}{"checkStrategy": "none"}
	if fromTemplate, ok, _ := unstructured.NestedMap(template.UnstructuredContent(), "spec", "template", "spec", "virtualMachineBootstrapCheck"); ok && len(fromTemplate) > 0 {
		bootstrapCheck = fromTemplate
	}
	if err := unstructured.SetNestedMap(kubevirtMachine.UnstructuredContent(), bootstrapCheck, "spec", "virtualMachineBootstrapCheck"); err != nil {
		return nil, fmt.Errorf("failed to set virtualMachineBootstrapCheck: %w", err)
	}

	// Copy the virtualMachineTemplate.spec from the template
	// Template structure: spec.template.spec.virtualMachineTemplate.spec
	// KubevirtMachine structure: spec.virtualMachineTemplate.spec
	if vmTemplateSpec, ok, _ := unstructured.NestedMap(template.UnstructuredContent(), "spec", "template", "spec", "virtualMachineTemplate", "spec"); ok {
		// Copy running field if present
		if running, ok, _ := unstructured.NestedBool(vmTemplateSpec, "running"); ok {
			if err := unstructured.SetNestedField(kubevirtMachine.UnstructuredContent(), running, "spec", "virtualMachineTemplate", "spec", "running"); err != nil {
				return nil, fmt.Errorf("failed to set running field: %w", err)
			}
		}

		// Remove cloudInitNoCloud volumes - CAPK will add CloudInitConfigDrive volume itself
		if templateSpec, ok := vmTemplateSpec["template"].(map[string]interface{}); ok {
			if volumes, ok := templateSpec["volumes"].([]interface{}); ok {
				filteredVolumes := []interface{}{}
				for _, vol := range volumes {
					if volMap, ok := vol.(map[string]interface{}); ok {
						// Skip cloudInitNoCloud volumes - CAPK will add its own CloudInitConfigDrive volume
						if _, hasCloudInitNoCloud := volMap["cloudInitNoCloud"]; !hasCloudInitNoCloud {
							filteredVolumes = append(filteredVolumes, vol)
						}
					} else {
						filteredVolumes = append(filteredVolumes, vol)
					}
				}
				templateSpec["volumes"] = filteredVolumes
			}
			// Also remove cloudinitdisk from disks if present - CAPK will add its own disk
			if devices, ok := templateSpec["domain"].(map[string]interface{}); ok {
				if devs, ok := devices["devices"].(map[string]interface{}); ok {
					if disks, ok := devs["disks"].([]interface{}); ok {
						filteredDisks := []interface{}{}
						for _, disk := range disks {
							if diskMap, ok := disk.(map[string]interface{}); ok {
								if name, ok := diskMap["name"].(string); ok && name == "cloudinitdisk" {
									// Skip cloudinitdisk - CAPK will add its own disk
									continue
								}
							}
							filteredDisks = append(filteredDisks, disk)
						}
						devs["disks"] = filteredDisks
					}
				}
			}
		}
		// Set as virtualMachineTemplate.spec in the KubevirtMachine
		if err := unstructured.SetNestedMap(kubevirtMachine.UnstructuredContent(), vmTemplateSpec, "spec", "virtualMachineTemplate", "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}
	}

	return kubevirtMachine, nil
}
