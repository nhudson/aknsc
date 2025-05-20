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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/nhudson/aknsc/api/v1alpha1"
)

// getCurrentNamespaceClassResources returns resources in the namespace that are managed by the NamespaceClass.
// It uses the discovery client to find all resource types, then lists resources with the appropriate label.
func (r *NamespaceClassReconciler) getCurrentNamespaceClassResources(ctx context.Context, namespace *corev1.Namespace, namespaceClass *v1alpha1.NamespaceClass) ([]string, error) {
	logger := logf.FromContext(ctx)
	logger.V(1).Info("Getting current managed resources",
		"namespace", namespace.Name,
		"namespaceClass", namespaceClass.Name)

	// Get preferred resources from discovery client
	resources, err := r.getServerPreferredResources()
	if err != nil {
		return nil, fmt.Errorf("failed to get server resources: %w", err)
	}

	// Estimate a reasonable capacity based on number of resource types
	estimatedCapacity := 0
	for _, resourceList := range resources {
		estimatedCapacity += len(resourceList.APIResources)
	}
	// Cap at a reasonable size to avoid huge allocations
	if estimatedCapacity > 100 {
		estimatedCapacity = 100
	}

	currentResources := make([]string, 0, estimatedCapacity)

	for _, resourceList := range resources {
		if len(resourceList.APIResources) == 0 {
			continue
		}

		groupVersion, err := schema.ParseGroupVersion(resourceList.GroupVersion)
		if err != nil {
			logger.Error(err, "Failed to parse group version, skipping",
				"groupVersion", resourceList.GroupVersion)
			continue
		}

		currentResources = append(currentResources, r.findManagedResourcesForGV(
			ctx, namespace, namespaceClass, groupVersion, resourceList.APIResources)...)
	}

	logger.V(1).Info("Found managed resources",
		"namespace", namespace.Name,
		"count", len(currentResources))
	return currentResources, nil
}

// findManagedResourcesForGV finds resources managed by the NamespaceClass for a specific GroupVersion.
func (r *NamespaceClassReconciler) findManagedResourcesForGV(
	ctx context.Context,
	namespace *corev1.Namespace,
	namespaceClass *v1alpha1.NamespaceClass,
	groupVersion schema.GroupVersion,
	apiResources []metav1.APIResource,
) []string {
	logger := logf.FromContext(ctx)

	resources := make([]string, 0, len(apiResources))

	for _, apiResource := range apiResources {
		// Skip resources we can't work with
		if !apiResource.Namespaced || !slices.Contains(apiResource.Verbs, "list") {
			continue
		}

		gvk := schema.GroupVersionKind{
			Group:   groupVersion.Group,
			Version: groupVersion.Version,
			Kind:    apiResource.Kind,
		}

		logger.V(2).Info("Checking resource type",
			"namespace", namespace.Name,
			"kind", gvk.Kind,
			"group", gvk.Group,
			"version", gvk.Version)

		// Find resources with our label
		resources = append(resources, r.listManagedResourcesOfKind(
			ctx, namespace, namespaceClass, gvk)...)
	}

	return resources
}

// listManagedResourcesOfKind lists resources of a specific kind managed by the NamespaceClass.
func (r *NamespaceClassReconciler) listManagedResourcesOfKind(
	ctx context.Context,
	namespace *corev1.Namespace,
	namespaceClass *v1alpha1.NamespaceClass,
	gvk schema.GroupVersionKind,
) []string {
	logger := logf.FromContext(ctx)

	resources := make([]string, 0, 5)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	// List resources in the namespace with our owner label
	err := r.List(ctx, list,
		client.InNamespace(namespace.Name),
		client.MatchingLabels{NamespaceClassOwner: namespaceClass.Name},
	)
	if err != nil {
		// Some types might not be retrievable, just log and continue
		logger.V(1).Info("Error listing resources, skipping",
			"namespace", namespace.Name,
			"kind", gvk.Kind,
			"group", gvk.Group,
			"version", gvk.Version,
			"error", err.Error())
		return resources
	}

	// Check capacity and resize if needed
	if cap(resources) < len(list.Items) {
		newResources := make([]string, 0, len(list.Items))
		resources = newResources
	}

	// Process found resources
	for _, item := range list.Items {
		resourceID := client.ObjectKeyFromObject(&item).String()
		logger.V(1).Info("Found managed resource",
			"namespace", namespace.Name,
			"resource", resourceID,
			"kind", item.GetKind())
		resources = append(resources,
			fmt.Sprintf("%s/%s/%s", gvk.Group, gvk.Kind, item.GetName()))
	}

	return resources
}

// applyResourceToNamespace applies a resource defined in a NamespaceClass to a namespace.
// It handles both creation of new resources and updates to existing ones.
func (r *NamespaceClassReconciler) applyResourceToNamespace(
	ctx context.Context,
	namespace *corev1.Namespace,
	namespaceClass *v1alpha1.NamespaceClass,
	resource runtime.RawExtension,
) error {
	logger := logf.FromContext(ctx)
	logger.V(1).Info("Starting to apply resource to namespace", "namespace", namespace.Name)

	// Convert the raw resource to unstructured
	unstructuredObj, err := convertRawToUnstructured(resource)
	if err != nil {
		return fmt.Errorf("failed to convert resource for namespace %s: %w", namespace.Name, err)
	}

	// Extract resource metadata for logging and reference
	gvk := unstructuredObj.GetObjectKind().GroupVersionKind()
	resourceName := unstructuredObj.GetName()
	resourceKind := gvk.Kind
	apiVersion := gvk.GroupVersion().String()

	logger.V(1).Info("Processing resource",
		"namespace", namespace.Name,
		"kind", resourceKind,
		"name", resourceName,
		"apiVersion", apiVersion)

	r.prepareResourceForNamespace(unstructuredObj, namespace, namespaceClass)

	if err := r.applyUnstructuredResource(ctx, unstructuredObj); err != nil {
		return fmt.Errorf("failed to apply resource %s/%s to namespace %s: %w",
			resourceKind, resourceName, namespace.Name, err)
	}

	logger.Info("Successfully applied resource",
		"namespace", namespace.Name,
		"kind", resourceKind,
		"name", resourceName)
	return nil
}

// prepareResourceForNamespace prepares a resource for application to a namespace.
// It sets the namespace and adds owner labels for tracking.
func (r *NamespaceClassReconciler) prepareResourceForNamespace(
	obj *unstructured.Unstructured,
	namespace *corev1.Namespace,
	namespaceClass *v1alpha1.NamespaceClass,
) {
	// Set the namespace for the resource
	obj.SetNamespace(namespace.Name)

	// Set the owner label for tracking
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[NamespaceClassOwner] = namespaceClass.Name
	obj.SetLabels(labels)
}

// applyUnstructuredResource creates or updates an unstructured resource.
// It handles the logic for determining if a resource exists and whether to create or update it.
func (r *NamespaceClassReconciler) applyUnstructuredResource(
	ctx context.Context,
	obj *unstructured.Unstructured,
) error {
	logger := logf.FromContext(ctx)
	namespace := obj.GetNamespace()
	resourceName := obj.GetName()
	resourceKind := obj.GetKind()

	// Check if the resource already exists
	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(obj.GroupVersionKind())

	err := r.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      resourceName,
	}, existingObj)

	if err == nil {
		return r.updateUnstructuredResource(ctx, obj, existingObj)
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("error checking if resource exists: %w", err)
	}

	// Resource doesn't exist, create it
	logger.Info("Creating new resource",
		"namespace", namespace,
		"kind", resourceKind,
		"name", resourceName)

	if err := r.Create(ctx, obj); err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	return nil
}

// updateUnstructuredResource updates an existing unstructured resource.
// It preserves the resourceVersion to avoid conflicts.
func (r *NamespaceClassReconciler) updateUnstructuredResource(
	ctx context.Context,
	newObj *unstructured.Unstructured,
	existingObj *unstructured.Unstructured,
) error {
	logger := logf.FromContext(ctx)
	namespace := newObj.GetNamespace()
	resourceName := newObj.GetName()
	resourceKind := newObj.GetKind()

	logger.Info("Updating existing resource",
		"namespace", namespace,
		"kind", resourceKind,
		"name", resourceName)

	// Preserve the resourceVersion to avoid conflicts
	newObj.SetResourceVersion(existingObj.GetResourceVersion())

	if err := r.Update(ctx, newObj); err != nil {
		return fmt.Errorf("failed to update resource: %w", err)
	}

	return nil
}

// convertRawToUnstructured converts a runtime.RawExtension to an unstructured.Unstructured object
func convertRawToUnstructured(raw runtime.RawExtension) (*unstructured.Unstructured, error) {
	unstructuredObj := &unstructured.Unstructured{}

	if raw.Object != nil {
		// If raw.Object is already set, convert it to unstructured
		data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(raw.Object)
		if err != nil {
			return nil, fmt.Errorf("failed to convert object to unstructured: %w", err)
		}

		unstructuredObj.SetUnstructuredContent(data)
		return unstructuredObj, nil
	} else if len(raw.Raw) > 0 {
		// If raw.Raw is set, unmarshal it to unstructured
		err := json.Unmarshal(raw.Raw, unstructuredObj)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal raw content to unstructured: %w", err)
		}

		// Validate that the unmarshalled object has the minimum required fields
		if unstructuredObj.GetName() == "" {
			return nil, fmt.Errorf("resource is missing required 'metadata.name' field")
		}

		if unstructuredObj.GetKind() == "" {
			return nil, fmt.Errorf("resource is missing required 'kind' field")
		}

		return unstructuredObj, nil
	}

	return nil, fmt.Errorf("raw extension contains no object data (both Object and Raw are nil/empty)")
}

// cleanupPreviousNamespaceClassResources removes resources from a previous NamespaceClass that are no longer needed.
// This is called when a namespace switches from one NamespaceClass to another.
func (r *NamespaceClassReconciler) cleanupPreviousNamespaceClassResources(
	ctx context.Context,
	namespace *corev1.Namespace,
	previousClassName string,
	currentClassName string,
) error {
	logger := logf.FromContext(ctx)
	logger.Info("Cleaning up resources from previous NamespaceClass",
		"namespace", namespace.Name,
		"previousClass", previousClassName,
		"currentClass", currentClassName)

	// Get the previous NamespaceClass definition
	previousClass := &v1alpha1.NamespaceClass{}
	err := r.Get(ctx, types.NamespacedName{Name: previousClassName}, previousClass)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Previous NamespaceClass no longer exists - we should clean up all resources with its label
			logger.Info("Previous NamespaceClass no longer exists, cleaning up all labeled resources",
				"namespace", namespace.Name,
				"previousClass", previousClassName)
			return r.cleanupAllNamespaceClassResources(ctx, namespace, previousClassName)
		}
		return fmt.Errorf("failed to get previous NamespaceClass %s: %w", previousClassName, err)
	}

	// Get current resources with the previous class's label
	prevResources, err := r.getResourcesWithOwner(ctx, namespace, previousClassName)
	if err != nil {
		return fmt.Errorf("failed to list resources for previous class: %w", err)
	}
	logger.Info("Found resources from previous NamespaceClass",
		"namespace", namespace.Name,
		"previousClass", previousClassName,
		"resourceCount", len(prevResources))

	// Get current NamespaceClass to determine which resources to preserve
	currentClass := &v1alpha1.NamespaceClass{}
	err = r.Get(ctx, types.NamespacedName{Name: currentClassName}, currentClass)
	if err != nil {
		return fmt.Errorf("failed to get current NamespaceClass %s: %w", currentClassName, err)
	}

	// Convert current class resources to a map of name+kind keys for efficient lookup
	currentResourceMap := make(map[string]bool)
	for _, res := range currentClass.Spec.Resources {
		obj, err := convertRawToUnstructured(res)
		if err != nil {
			logger.Error(err, "Failed to convert resource, skipping",
				"namespace", namespace.Name,
				"class", currentClassName)
			continue
		}
		key := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
		currentResourceMap[key] = true
	}

	// Remove resources from the previous class that aren't in the current class
	for _, res := range prevResources {
		parts := strings.Split(res, "/")
		if len(parts) < 3 {
			logger.Error(nil, "Invalid resource identifier format, skipping",
				"resource", res,
				"namespace", namespace.Name)
			continue
		}
		kind := parts[1]
		name := parts[2]

		// Check if this resource exists in the current class
		key := fmt.Sprintf("%s/%s", kind, name)
		if currentResourceMap[key] {
			logger.V(1).Info("Resource exists in new class, skipping deletion",
				"resource", key,
				"namespace", namespace.Name)
			continue
		}

		// Resource not in current class, delete it
		logger.Info("Removing resource from previous NamespaceClass",
			"resource", key,
			"namespace", namespace.Name,
			"previousClass", previousClassName)

		if err := r.deleteResourceByKindAndName(ctx, namespace.Name, kind, name, parts[0]); err != nil {
			logger.Error(err, "Failed to delete resource",
				"resource", key,
				"namespace", namespace.Name)
			continue
		}
	}

	logger.Info("Successfully cleaned up resources from previous NamespaceClass",
		"namespace", namespace.Name,
		"previousClass", previousClassName)
	return nil
}

// getResourcesWithOwner returns a list of resources with a specific NamespaceClass owner label.
func (r *NamespaceClassReconciler) getResourcesWithOwner(
	ctx context.Context,
	namespace *corev1.Namespace,
	className string,
) ([]string, error) {
	placeholderClass := &v1alpha1.NamespaceClass{}
	placeholderClass.Name = className

	return r.getCurrentNamespaceClassResources(ctx, namespace, placeholderClass)
}

// deleteResourceByKindAndName deletes a resource of a specific kind and name.
func (r *NamespaceClassReconciler) deleteResourceByKindAndName(
	ctx context.Context,
	namespace string,
	kind string,
	name string,
	group string,
) error {
	obj := &unstructured.Unstructured{}

	// Try to get the preferred version for this resource type
	preferredVersion := "v1"
	if group != "" {
		versions, err := r.getPreferredVersionForResource(ctx, group, kind)
		if err == nil && len(versions) > 0 {
			// Use the first preferred version
			preferredVersion = versions[0]
		}
	}

	gvk := schema.GroupVersionKind{
		Group:   group,
		Version: preferredVersion,
		Kind:    kind,
	}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)

	// Delete the resource
	err := r.Delete(ctx, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete %s/%s: %w", kind, name, err)
	}
	return nil
}

// getPreferredVersionForResource returns preferred API versions for a resource type.
func (r *NamespaceClassReconciler) getPreferredVersionForResource(
	ctx context.Context,
	group string,
	kind string,
) ([]string, error) {
	logger := logf.FromContext(ctx)

	// Get all API resources in the cluster
	resourceLists, err := r.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		discErr, ok := err.(*discovery.ErrGroupDiscoveryFailed)
		if ok {
			// Log the specific groups that failed
			for gv, err := range discErr.Groups {
				logger.Error(err, "Failed to discover API group", "group", gv)
			}
			// Continue with the resources we were able to get
			if len(resourceLists) == 0 {
				return nil, fmt.Errorf("no resources discovered")
			}
		} else {
			return nil, err
		}
	}

	var versions []string

	// Find the preferred version for this group/kind
	for _, resourceList := range resourceLists {
		if len(resourceList.APIResources) == 0 {
			continue
		}

		// Parse group and version
		gv, err := schema.ParseGroupVersion(resourceList.GroupVersion)
		if err != nil {
			continue
		}

		// Check if this is the right group
		if gv.Group != group {
			continue
		}

		// Look for the kind in this group/version
		for _, res := range resourceList.APIResources {
			if res.Kind == kind {
				versions = append(versions, gv.Version)
				break
			}
		}
	}

	return versions, nil
}
