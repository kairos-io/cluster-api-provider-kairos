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

package bootstrap

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	bootstrapv1beta2 "github.com/kairos-io/kairos-capi/api/bootstrap/v1beta2"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// TemplateData holds data for rendering the Kairos cloud-config template
type TemplateData struct {
	Role                           string
	SingleNode                     bool
	Hostname                       string
	UserName                       string
	UserPassword                   string
	UserGroups                     []string
	GitHubUser                     string
	SSHPublicKey                   string
	WorkerToken                    string
	Manifests                      []bootstrapv1beta2.Manifest
	HostnamePrefix                 string
	DNSServers                     []string
	PodCIDR                        string
	ServiceCIDR                    string
	PrimaryIP                      string
	MachineName                    string
	ClusterNS                      string
	IsKubeVirt                     bool
	Install                        *InstallConfig
	ProviderID                     string // ProviderID for the Node (e.g., "vsphere://<vm-uuid>")
	K3sServerURL                   string
	K3sToken                       string
	ControlPlaneLBServiceName      string
	ControlPlaneLBServiceNamespace string
	ControlPlaneLBEndpoint         string
	// Management cluster push config for KubeVirt (non-SSH kubeconfig retrieval)
	ManagementKubeconfigToken           string
	ManagementKubeconfigSecretName      string
	ManagementKubeconfigSecretNamespace string
	ManagementAPIServer                 string
}

// InstallConfig holds installation configuration for the template
type InstallConfig struct {
	Auto   bool
	Device string
	Reboot bool
}

// RenderK0sCloudConfig renders the k0s Kairos cloud-config template
func RenderK0sCloudConfig(data TemplateData) (string, error) {
	// Load template (split per provider)
	templatePath := "templates/k0s_kairos_cloud_config_capv.yaml.tmpl"
	if data.IsKubeVirt {
		templatePath = "templates/k0s_kairos_cloud_config_capk.yaml.tmpl"
	}
	tmplContent, err := templateFS.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template: %w", err)
	}

	// Create template with custom functions
	tmpl := template.New("k0s_kairos_cloud_config").Funcs(template.FuncMap{
		"indent": func(spaces int, s string) string {
			if s == "" {
				return ""
			}
			indent := strings.Repeat(" ", spaces)
			lines := strings.Split(s, "\n")
			var result []string
			for _, line := range lines {
				if line != "" {
					result = append(result, indent+line)
				} else {
					result = append(result, "")
				}
			}
			return strings.Join(result, "\n")
		},
		"trimSuffix": func(suffix, s string) string {
			return strings.TrimSuffix(s, suffix)
		},
	})

	// Parse template
	tmpl, err = tmpl.Parse(string(tmplContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Render template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// RenderK3sCloudConfig renders the k3s Kairos cloud-config template
func RenderK3sCloudConfig(data TemplateData) (string, error) {
	// Load template (split per provider)
	templatePath := "templates/k3s_kairos_cloud_config_capv.yaml.tmpl"
	if data.IsKubeVirt {
		templatePath = "templates/k3s_kairos_cloud_config_capk.yaml.tmpl"
	}
	tmplContent, err := templateFS.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template: %w", err)
	}

	// Create template with custom functions
	tmpl := template.New("k3s_kairos_cloud_config").Funcs(template.FuncMap{
		"indent": func(spaces int, s string) string {
			if s == "" {
				return ""
			}
			indent := strings.Repeat(" ", spaces)
			lines := strings.Split(s, "\n")
			var result []string
			for _, line := range lines {
				if line != "" {
					result = append(result, indent+line)
				} else {
					result = append(result, "")
				}
			}
			return strings.Join(result, "\n")
		},
		"trimSuffix": func(suffix, s string) string {
			return strings.TrimSuffix(s, suffix)
		},
	})

	// Parse template
	tmpl, err = tmpl.Parse(string(tmplContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Render template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}
