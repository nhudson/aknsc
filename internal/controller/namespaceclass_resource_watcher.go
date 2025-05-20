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
	"fmt"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/nhudson/aknsc/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ResourceWatchResult contains the results of setting up resource watchers
type ResourceWatchResult struct {
	// WatchedTypes is a list of GroupVersionKinds successfully watched
	WatchedTypes []schema.GroupVersionKind
	// FailedTypes is a list of GroupVersionKinds that failed to be watched
	FailedTypes []schema.GroupVersionKind
}

// setupResourceWatchers sets up watches for resource types that may be managed by a NamespaceClass.
// It dynamically discovers all namespaced resource types in the cluster and sets up watches for them.
func (r *NamespaceClassReconciler) setupResourceWatchers(
	mgr ctrl.Manager,
	resourcePredicate predicate.Predicate,
	eventHandler handler.EventHandler,
) error {
	logger := logf.FromContext(context.Background()).WithName("resource-watcher")
	logger.Info("Setting up resource watchers for resources with NamespaceClass owner label",
		"ownerLabel", NamespaceClassOwner)

	// Discover and watch all resource types
	result, err := r.watchDiscoveredResources(mgr, resourcePredicate, eventHandler)
	if err != nil {
		logger.Error(err, "Error discovering and watching resources")
	}

	// Log summary information
	r.logWatchSummary(logger, result)

	return nil
}

// watchDiscoveredResources uses the discovery client to find and watch all suitable resources
func (r *NamespaceClassReconciler) watchDiscoveredResources(
	mgr ctrl.Manager,
	resourcePredicate predicate.Predicate,
	eventHandler handler.EventHandler,
) (ResourceWatchResult, error) {
	logger := logf.FromContext(context.Background()).WithName("resource-watcher")
	result := ResourceWatchResult{
		WatchedTypes: make([]schema.GroupVersionKind, 0),
		FailedTypes:  make([]schema.GroupVersionKind, 0),
	}

	// Get all API resources in the cluster
	resourceLists, err := r.getServerPreferredResources()
	if err != nil {
		return result, err
	}

	// Process each resource list
	for _, resourceList := range resourceLists {
		if len(resourceList.APIResources) == 0 {
			continue
		}

		gv, err := schema.ParseGroupVersion(resourceList.GroupVersion)
		if err != nil {
			logger.Error(err, "Failed to parse group version", "groupVersion", resourceList.GroupVersion)
			continue
		}

		r.watchResourcesInGroupVersion(mgr, resourcePredicate, eventHandler, gv, resourceList.APIResources, &result)
	}

	return result, nil
}

// getServerPreferredResources gets server resources with graceful handling of partial discovery failures
func (r *NamespaceClassReconciler) getServerPreferredResources() ([]*metav1.APIResourceList, error) {
	logger := logf.FromContext(context.Background()).WithName("resource-watcher")

	resourceLists, err := r.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		// Handle partial discovery errors gracefully
		discErr, ok := err.(*discovery.ErrGroupDiscoveryFailed)
		if !ok {
			return nil, fmt.Errorf("failed to get server resources: %w", err)
		}

		// Log groups that failed
		for gv, err := range discErr.Groups {
			logger.Error(err, "Failed to discover API group", "group", gv)
		}

		// Continue with what we have if we got anything
		if len(resourceLists) == 0 {
			return nil, fmt.Errorf("failed to discover any API resources")
		}

		logger.Info("Continuing with partial discovery results",
			"successfulGroups", len(resourceLists),
			"failedGroups", len(discErr.Groups))
	}

	return resourceLists, nil
}

// watchResourcesInGroupVersion sets up watches for suitable resources in a specific GroupVersion
func (r *NamespaceClassReconciler) watchResourcesInGroupVersion(
	mgr ctrl.Manager,
	resourcePredicate predicate.Predicate,
	eventHandler handler.EventHandler,
	gv schema.GroupVersion,
	apiResources []metav1.APIResource,
	result *ResourceWatchResult,
) {
	logger := logf.FromContext(context.Background()).WithName("resource-watcher")

	for _, apiResource := range apiResources {
		if !shouldWatchResource(apiResource) {
			continue
		}

		gvk := schema.GroupVersionKind{
			Group:   gv.Group,
			Version: gv.Version,
			Kind:    apiResource.Kind,
		}

		// Create the watch
		err := r.createWatchForGVK(mgr, resourcePredicate, eventHandler, gvk)
		if err != nil {
			logger.Error(err, "Failed to set up watch",
				"kind", gvk.Kind,
				"group", gvk.Group,
				"version", gvk.Version)
			result.FailedTypes = append(result.FailedTypes, gvk)
		} else {
			logger.Info("Set up watch for resource type",
				"kind", gvk.Kind,
				"group", gvk.Group,
				"version", gvk.Version)
			result.WatchedTypes = append(result.WatchedTypes, gvk)
		}
	}
}

// shouldWatchResource determines if a resource should be watched based on its properties
func shouldWatchResource(apiResource metav1.APIResource) bool {
	// Only watch namespaced resources
	if !apiResource.Namespaced {
		return false
	}

	// Only watch resources that support the watch verb
	if !slices.Contains(apiResource.Verbs, "watch") {
		return false
	}

	// Skip subresources (like pod/log, pod/exec)
	if strings.Contains(apiResource.Name, "/") {
		return false
	}

	return true
}

// createWatchForGVK creates a controller watch for a specific GroupVersionKind
func (r *NamespaceClassReconciler) createWatchForGVK(
	mgr ctrl.Manager,
	resourcePredicate predicate.Predicate,
	eventHandler handler.EventHandler,
	gvk schema.GroupVersionKind,
) error {
	if gvk.Empty() {
		return fmt.Errorf("empty GroupVersionKind")
	}

	// Create an Unstructured object for this type
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	// Create a unique controller name
	controllerName := fmt.Sprintf("namespaceclass-%s-%s-%s-watch",
		normalizeName(gvk.Group),
		normalizeName(gvk.Version),
		normalizeName(gvk.Kind))

	// Limit controller name length (kubernetes has a 63 character limit)
	maxNameLength := 60
	if len(controllerName) > maxNameLength {
		controllerName = controllerName[:maxNameLength]
	}

	// Create controller to watch this resource type
	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.NamespaceClass{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(obj, eventHandler, builder.WithPredicates(resourcePredicate))

	return controller.Named(controllerName).Complete(r)
}

// normalizeName normalizes a name for use in controller names
func normalizeName(name string) string {
	// Replace any dots with dashes
	normalized := strings.ReplaceAll(name, ".", "-")

	// If empty, use "core"
	if normalized == "" {
		return "core"
	}

	return strings.ToLower(normalized)
}

// logWatchSummary logs a summary of watched and failed resource types
func (r *NamespaceClassReconciler) logWatchSummary(logger logr.Logger, result ResourceWatchResult) {
	logger.Info("Resource watcher setup summary",
		"watched_count", len(result.WatchedTypes),
		"failed_count", len(result.FailedTypes))

	// Log watched types
	if len(result.WatchedTypes) > 0 {
		watchedStrings := formatGVKsForLogging(result.WatchedTypes)
		logger.Info("Watched resource types", "types", strings.Join(watchedStrings, ", "))
	}

	// Log failed types
	if len(result.FailedTypes) > 0 {
		failedStrings := formatGVKsForLogging(result.FailedTypes)
		logger.Info("Failed to watch resource types", "types", strings.Join(failedStrings, ", "))
	}
}

// formatGVKsForLogging formats a list of GVKs for logging, with a maximum number to avoid too verbose logs
func formatGVKsForLogging(gvks []schema.GroupVersionKind) []string {
	maxToLog := 10
	count := min(len(gvks), maxToLog)
	result := make([]string, 0, count)

	for i, gvk := range gvks {
		if i >= maxToLog {
			break
		}
		if gvk.Group == "" {
			result = append(result, gvk.Kind)
		} else {
			result = append(result, fmt.Sprintf("%s.%s", gvk.Kind, gvk.Group))
		}
	}

	if len(gvks) > maxToLog {
		result = append(result, fmt.Sprintf("and %d more", len(gvks)-maxToLog))
	}

	return result
}
