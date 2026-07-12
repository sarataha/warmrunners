/*
Copyright 2026.

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

// Package v1alpha1 contains API Schema definitions for the warmrunners v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=autoscaling.warmrunners.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "autoscaling.warmrunners.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	// controller-runtime v0.24 deprecated scheme.Builder in favour of the
	// leaner apimachinery runtime.SchemeBuilder, but the kubebuilder scaffold
	// (and every existing types file's init() Register call) still targets
	// this helper. Migration is a wider refactor tracked separately.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // kubebuilder scaffold, deferred migration

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
