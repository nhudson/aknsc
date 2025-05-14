/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// NamespaceClassSpec defines the desired state of NamespaceClass.
type NamespaceClassSpec struct {
	// Resources contains a list of Kubernetes resource manifests to be applied to namespaces
	// using this class. Each item should be a valid Kubernetes resource definition.
	Resources []runtime.RawExtension `json:"resources,omitempty"`
}

// NamespaceClassNamespaceStatus represents the status of a namespace with respect to this NamespaceClass.
type NamespaceClassNamespaceStatus struct {
	// Name is the name of the namespace.
	Name string `json:"name"`
	// CurrentClass is the current class label on the namespace (if any).
	CurrentClass string `json:"currentClass,omitempty"`
	// LastAppliedClass is the last class applied by the controller (if any).
	LastAppliedClass string `json:"lastAppliedClass,omitempty"`
	// ResourceStatus can be used to track the state of resources (optional, for future use).
	ResourceStatus string `json:"resourceStatus,omitempty"`
}

// NamespaceClassStatus defines the observed state of NamespaceClass.
// This struct is updated by the controller to reflect the current state of the system.
type NamespaceClassStatus struct {
	// ObservedGeneration is the most recent generation observed for this NamespaceClass.
	// It is used to determine if the controller has processed the latest spec changes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Namespaces contains the status of each namespace using or previously using this class.
	Namespaces []NamespaceClassNamespaceStatus `json:"namespaces,omitempty"`
	// Conditions represent the latest available observations of the object's state.
	// Standard Kubernetes conditions such as Ready, Error, or ResourcesApplied may be used.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// NamespaceClass is the Schema for the namespaceclasses API.
type NamespaceClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NamespaceClassSpec   `json:"spec,omitempty"`
	Status NamespaceClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NamespaceClassList contains a list of NamespaceClass.
type NamespaceClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceClass{}, &NamespaceClassList{})
}
