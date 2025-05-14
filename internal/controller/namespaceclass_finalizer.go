package controller

import (
	"context"
	"errors"
	"strings"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/nhudson/aknsc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// reconcileFinalizer ensures the NamespaceClass finalizer is present and handles cleanup on deletion.
func (r *NamespaceClassReconciler) reconcileNamespaceClassFinalizer(ctx context.Context, nc *v1alpha1.NamespaceClass, contextLogger logr.Logger) (ctrl.Result, error) {
	if nc.ObjectMeta.DeletionTimestamp.IsZero() {
		// Resource is NOT being deleted: ensure finalizer is present
		return r.ensureFinalizerPresent(ctx, nc, contextLogger)
	} else {
		// Resource IS being deleted: handle cleanup and remove finalizer
		return r.handleFinalizerCleanup(ctx, nc, contextLogger)
	}
}

// ensureFinalizerPresent adds the NamespaceClassFinalizer to the resource if not already present.
func (r *NamespaceClassReconciler) ensureFinalizerPresent(ctx context.Context, nc *v1alpha1.NamespaceClass, contextLogger logr.Logger) (ctrl.Result, error) {
	for _, f := range nc.Finalizers {
		if f == NamespaceClassFinalizer {
			return ctrl.Result{}, nil
		}
	}
	// Add the finalizer
	nc.Finalizers = append(nc.Finalizers, NamespaceClassFinalizer)
	if err := r.Update(ctx, nc); err != nil {
		contextLogger.Error(err, "Failed to add finalizer")
		return ctrl.Result{}, err
	}
	contextLogger.Info("Added finalizer to NamespaceClass")
	return ctrl.Result{Requeue: true}, nil
}

// handleFinalizerCleanup is called when the NamespaceClass is being deleted and the finalizer is present.
// It should clean up any resources created by this NamespaceClass, then remove the finalizer so deletion can proceed.
func (r *NamespaceClassReconciler) handleFinalizerCleanup(ctx context.Context, nc *v1alpha1.NamespaceClass, contextLogger logr.Logger) (ctrl.Result, error) {
	var errs []error

	// Clean up all resources in all namespaces that use this NamespaceClass
	nsList := &corev1.NamespaceList{}
	if err := r.List(ctx, nsList, client.MatchingLabels{NamespaceClassLabel: nc.Name}); err != nil {
		contextLogger.Error(err, "Failed to list namespaces for NamespaceClass cleanup", "class", nc.Name)
		errs = append(errs, err)
	} else {
		for _, ns := range nsList.Items {
			// Skip cleanup for namespaces that are being deleted, as the resources will be garbage collected
			if ns.DeletionTimestamp != nil {
				contextLogger.Info("Skipping cleanup for namespace that is already being deleted",
					"namespace", ns.Name,
					"class", nc.Name)
				continue
			}

			// Find all resources with this NamespaceClass owner label
			resources, err := r.listResourcesWithLabel(ctx, ns.Name, NamespaceClassOwner, nc.Name)
			if err != nil {
				contextLogger.Error(err, "Failed to list resources for cleanup",
					"namespace", ns.Name,
					"class", nc.Name)
				errs = append(errs, err)
				continue
			}

			// Delete each resource
			for _, resource := range resources {
				// Get resource kind and name
				var kind, name string

				// Check if it's an unstructured resource
				if u, ok := resource.(*unstructured.Unstructured); ok {
					kind = u.GetKind()
					name = u.GetName()
				} else {
					// Determine kind and name using reflection or type assertion
					kind = "Resource"
					name = resource.GetName()
				}

				contextLogger.Info("Deleting resource during NamespaceClass cleanup",
					"namespace", ns.Name,
					"kind", kind,
					"name", name)

				if err := r.Client.Delete(ctx, resource); err != nil {
					contextLogger.Error(err, "Failed to delete resource during cleanup",
						"namespace", ns.Name,
						"kind", kind,
						"name", name)
					errs = append(errs, err)
				}
			}

			// Update the namespace to remove annotations
			nsToUpdate := &corev1.Namespace{}
			if err := r.Client.Get(ctx, types.NamespacedName{Name: ns.Name}, nsToUpdate); err != nil {
				contextLogger.Error(err, "Failed to get namespace for annotation cleanup", "namespace", ns.Name)
				errs = append(errs, err)
				continue
			}

			// Remove any annotations related to this NamespaceClass
			if nsToUpdate.Annotations != nil {
				delete(nsToUpdate.Annotations, LastAppliedClassAnnotation)
				if err := r.Client.Update(ctx, nsToUpdate); err != nil {
					contextLogger.Error(err, "Failed to update namespace annotations during cleanup", "namespace", ns.Name)
					errs = append(errs, err)
				}
			}
		}
	}

	// Remove the finalizer to allow deletion to proceed
	hasNamespaceClassFinalizer := false
	for i, f := range nc.Finalizers {
		if f == NamespaceClassFinalizer {
			contextLogger.Info("Removing finalizer from NamespaceClass", "class", nc.Name)
			// Remove the finalizer from the list
			nc.Finalizers = append(nc.Finalizers[:i], nc.Finalizers[i+1:]...)
			hasNamespaceClassFinalizer = true
			break
		}
	}

	if hasNamespaceClassFinalizer {
		contextLogger.Info("Removing finalizer from NamespaceClass", "class", nc.Name)
		if err := r.Update(ctx, nc); err != nil {
			contextLogger.Error(err, "Failed to remove finalizer from NamespaceClass", "class", nc.Name)
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}

	contextLogger.Info("Successfully cleaned up resources for NamespaceClass", "class", nc.Name)
	return ctrl.Result{}, nil
}

// listResourcesWithLabel lists all resources in a namespace that have a specific label key/value pair
func (r *NamespaceClassReconciler) listResourcesWithLabel(ctx context.Context, namespace string, labelKey string, labelValue string) ([]client.Object, error) {
	logger := logf.FromContext(ctx)

	// Create a label selector for the owner label
	labelSelector := client.MatchingLabels{
		labelKey: labelValue,
	}

	var allResources []client.Object

	// Get all API resources in the cluster that we can query
	resourceLists, err := r.getServerPreferredResources()
	if err != nil {
		logger.Error(err, "Failed to get server resources")
		// Continue with an empty list if we can't get resources
	}

	// Process each resource list
	for _, resourceList := range resourceLists {
		// Skip empty API groups
		if len(resourceList.APIResources) == 0 {
			continue
		}

		groupVersion, err := schema.ParseGroupVersion(resourceList.GroupVersion)
		if err != nil {
			logger.Error(err, "Failed to parse group version", "groupVersion", resourceList.GroupVersion)
			continue
		}

		// Process each resource in this group/version
		for _, resource := range resourceList.APIResources {
			// Skip resources that don't meet our criteria
			if !resource.Namespaced || !containsString(resource.Verbs, "list") {
				continue
			}

			// Skip subresources (like pod/log, pod/exec)
			if strings.Contains(resource.Name, "/") {
				continue
			}

			// Create an unstructured list for this resource type
			listObj := &unstructured.UnstructuredList{}
			listObj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   groupVersion.Group,
				Version: groupVersion.Version,
				Kind:    resource.Kind + "List", // ListKind
			})

			// List resources of this type with our label
			err := r.Client.List(ctx, listObj, client.InNamespace(namespace), labelSelector)
			if err != nil {
				// Skip resources that we can't list (likely due to permissions)
				logger.V(1).Info("Failed to list resources of type, skipping",
					"kind", resource.Kind,
					"group", groupVersion.Group,
					"version", groupVersion.Version,
					"error", err.Error())
				continue
			}

			// Add found resources to our result
			for i := range listObj.Items {
				item := listObj.Items[i]
				allResources = append(allResources, &item)
			}
		}
	}

	logger.Info("Found resources with label",
		"namespace", namespace,
		"labelKey", labelKey,
		"labelValue", labelValue,
		"count", len(allResources))

	// Log a summary of what we found
	resourceSummary := map[string]int{}
	for _, obj := range allResources {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if kind == "" {
			kind = "Unknown"
		}
		resourceSummary[kind]++
	}

	for kind, count := range resourceSummary {
		logger.Info("Resource summary", "kind", kind, "count", count)
	}

	return allResources, nil
}
