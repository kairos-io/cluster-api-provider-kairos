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

package controlplane

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// makeMetal3Machine builds a fake Metal3Machine with a status.addresses slice
// that matches the MachineAddresses shape used by CAPV and CAPM3.
func makeMetal3Machine(name, namespace string, addresses []map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "Metal3Machine",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if len(addresses) > 0 {
		addrs := make([]interface{}, len(addresses))
		for i, a := range addresses {
			addrs[i] = a
		}
		_ = unstructured.SetNestedSlice(obj.Object, addrs, "status", "addresses")
	}

	return obj
}

func TestGetNodeIP_Metal3Machine(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bootstrapv1beta2.AddToScheme(scheme)
	_ = controlplanev1beta2.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tests := []struct {
		name      string
		addresses []map[string]interface{}
		wantIP    string
		wantErr   bool
	}{
		{
			name: "InternalIP is preferred",
			addresses: []map[string]interface{}{
				{"type": "ExternalIP", "address": "1.2.3.4"},
				{"type": "InternalIP", "address": "10.0.0.5"},
			},
			wantIP:  "10.0.0.5",
			wantErr: false,
		},
		{
			name: "ExternalIP used when no InternalIP",
			addresses: []map[string]interface{}{
				{"type": "ExternalIP", "address": "1.2.3.4"},
			},
			wantIP:  "1.2.3.4",
			wantErr: false,
		},
		{
			name: "first untyped address used when no InternalIP or ExternalIP",
			addresses: []map[string]interface{}{
				{"type": "", "address": "172.16.0.1"},
				{"type": "Hostname", "address": "node.example.com"},
			},
			wantIP:  "172.16.0.1",
			wantErr: false,
		},
		{
			name:      "no addresses returns error",
			addresses: nil,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m3m := makeMetal3Machine("test-m3m", "default", tc.addresses)

			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
				Spec: clusterv1.MachineSpec{
					InfrastructureRef: corev1.ObjectReference{
						APIVersion: "infrastructure.cluster.x-k8s.io/v1beta2",
						Kind:       "Metal3Machine",
						Name:       "test-m3m",
						Namespace:  "default",
					},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(m3m).
				Build()

			r := &KairosControlPlaneReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			ip, err := r.getNodeIP(context.Background(), log.Log, machine)
			if tc.wantErr {
				if err == nil {
					t.Errorf("getNodeIP() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("getNodeIP() unexpected error: %v", err)
			}
			if ip != tc.wantIP {
				t.Errorf("getNodeIP() = %q, want %q", ip, tc.wantIP)
			}
		})
	}
}
