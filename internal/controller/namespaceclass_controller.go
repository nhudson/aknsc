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
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/nhudson/aknsc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// NamespaceClassReconciler reconciles a NamespaceClass object
type NamespaceClassReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	DiscoveryClient *discovery.DiscoveryClient

	// mutex to prevent concurrent updates to namespace annotations
	namespaceMutex sync.Map
}

// +kubebuilder:rbac:groups=nhudson.dev,resources=namespaceclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nhudson.dev,resources=namespaceclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=akuity.io,resources=namespaceclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;create;update;patch;delete

const (
	// NamespaceClassLabel is the label key for associating a Namespace with a NamespaceClass.
	NamespaceClassLabel = "namespaceclass.akuity.io/name"

	// NamespaceClassFinalizer is the finalizer key for NamespaceClass resources.
	NamespaceClassFinalizer = "namespaceclass.akuity.io/finalizer"

	// NamespaceClassOwner is the label key for associating a resource with a NamespaceClass.
	NamespaceClassOwner = "namespaceclass.akuity.io/owner"

	// LastAppliedClassAnnotation is the annotation key for tracking the last applied NamespaceClass on a Namespace.
	LastAppliedClassAnnotation = "namespaceclass.akuity.io/last-applied-class"

	// CurrentClassAnnotation is the annotation key for tracking the currently applied NamespaceClass on a Namespace.
	CurrentClassAnnotation = "namespaceclass.akuity.io/current-class"
)

func (r *NamespaceClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Starting reconciliation", "object", req.Name)

	// First check if the request is for a NamespaceClass object
	namespaceClass := &v1alpha1.NamespaceClass{}
	err := r.Get(ctx, req.NamespacedName, namespaceClass)
	if err == nil {
		// This is a NamespaceClass reconciliation request
		logger.Info("Reconciling NamespaceClass", "name", namespaceClass.Name)

		// Find all namespaces with this class
		namespaceList := &corev1.NamespaceList{}
		if err = r.List(ctx, namespaceList, client.MatchingLabels{
			NamespaceClassLabel: namespaceClass.Name,
		}); err != nil {
			logger.Error(err, "Failed to list namespaces with class label",
				"namespaceClass", namespaceClass.Name)
			return ctrl.Result{}, err
		}

		// Reconcile each namespace that uses this class
		for i := range namespaceList.Items {
			namespace := &namespaceList.Items[i]
			logger.Info("Reconciling namespace using this class",
				"namespace", namespace.Name,
				"namespaceClass", namespaceClass.Name)

			// Reconcile this namespace
			if err = r.reconcileNamespaceWithClass(ctx, namespace, namespaceClass); err != nil {
				logger.Error(err, "Failed to reconcile namespace with class",
					"namespace", namespace.Name,
					"namespaceClass", namespaceClass.Name)
			}
		}

		// Process finalizer for NamespaceClass
		if !namespaceClass.DeletionTimestamp.IsZero() {
			var result ctrl.Result
			result, err = r.reconcileNamespaceClassFinalizer(ctx, namespaceClass, logger)
			if result.Requeue || err != nil {
				return result, err
			}
		}

		// Update status to track namespaces using this class
		if err = r.updateNamespaceClassStatus(ctx, namespaceClass); err != nil {
			logger.Error(err, "Failed to update NamespaceClass status")
			return ctrl.Result{}, err
		}

		logger.Info("Successfully reconciled NamespaceClass")
		return ctrl.Result{}, nil
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to get NamespaceClass", "name", req.Name)
		return ctrl.Result{}, err
	}

	// If we get here, it's a Namespace reconciliation request
	namespace, err := r.getNamespace(ctx, req.Name)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			logger.Error(err, "unable to fetch Namespace")
			return ctrl.Result{}, err
		}
		// Namespace not found, it might have been deleted
		logger.Info("Namespace not found, ignoring", "namespace", req.Name)
		return ctrl.Result{}, nil
	}

	// Check if namespace is being deleted, if so, don't try to reconcile resources
	if namespace.DeletionTimestamp != nil {
		logger.Info("Namespace is being deleted, skipping reconciliation", "namespace", namespace.Name)
		return ctrl.Result{}, nil
	}

	// Log the current state of the namespace
	logger.Info("Current namespace state",
		"namespace", namespace.Name,
		"labels", namespace.Labels,
		"annotations", namespace.Annotations)

	// Get the current NamespaceClass for this Namespace based on the label
	currentNamespaceClass, err := r.getNamespaceClass(ctx, namespace)
	if err != nil {
		logger.Error(err, "failed to get NamespaceClass", "namespace", namespace.Name)
		return ctrl.Result{}, err
	}

	// If currentNamespaceClass is nil, no reconciliation needed
	if currentNamespaceClass == nil {
		logger.Info("No NamespaceClass found for namespace, skipping", "namespace", namespace.Name)
		return ctrl.Result{}, nil
	}

	// Reconcile the namespace with its class
	if err := r.reconcileNamespaceWithClass(ctx, namespace, currentNamespaceClass); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled namespace with NamespaceClass",
		"namespace", namespace.Name,
		"namespaceClass", currentNamespaceClass.Name)
	return ctrl.Result{}, nil
}

// reconcileNamespaceWithClass handles the reconciliation of a namespace with its NamespaceClass
func (r *NamespaceClassReconciler) reconcileNamespaceWithClass(ctx context.Context, namespace *corev1.Namespace, currentNamespaceClass *v1alpha1.NamespaceClass) error {
	logger := logf.FromContext(ctx)

	// Get the most up-to-date namespace state
	latestNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace.Name}, latestNamespace); err != nil {
		return fmt.Errorf("failed to get latest namespace state: %w", err)
	}

	// Check for both original and current class annotations
	var originalClass, currentClass string
	if latestNamespace.Annotations != nil {
		originalClass = latestNamespace.Annotations[LastAppliedClassAnnotation]
		currentClass = latestNamespace.Annotations[CurrentClassAnnotation]
	}

	logger.Info("Namespace class state",
		"namespace", latestNamespace.Name,
		"targetClass", currentNamespaceClass.Name,
		"originalClass", originalClass,
		"currentClass", currentClass,
		"label", latestNamespace.Labels[NamespaceClassLabel])

	// Only set the original class annotation if it doesn't exist yet (first time setup)
	if originalClass == "" {
		// No original class recorded, this is the first application
		logger.Info("Setting original class annotation for first-time setup",
			"namespace", latestNamespace.Name,
			"class", currentNamespaceClass.Name)

		// Set both original and current class to the same value initially
		if err := r.setClassAnnotations(ctx, latestNamespace, currentNamespaceClass.Name, currentNamespaceClass.Name); err != nil {
			return err
		}

		// Refresh current values after update
		currentClass = currentNamespaceClass.Name
	}

	// Track previous class for status updates
	previousClass := currentClass

	// Detect class switch by comparing current class with target class
	if currentClass != "" && currentClass != currentNamespaceClass.Name {
		logger.Info("Detected NamespaceClass switch - CLEANUP NEEDED",
			"namespace", latestNamespace.Name,
			"previousClass", currentClass,
			"targetClass", currentNamespaceClass.Name,
			"originalClass", originalClass)

		// Store the previous class for status update before cleaning up
		previousClass = currentClass

		// Clean up resources from the current class before switching
		if err := r.cleanupPreviousNamespaceClassResources(ctx, latestNamespace, currentClass, currentNamespaceClass.Name); err != nil {
			logger.Error(err, "Failed to clean up resources from previous NamespaceClass",
				"namespace", latestNamespace.Name,
				"currentClass", currentClass)
			return err
		}

		// After cleanup, update the current class annotation
		if err := r.updateCurrentClassAnnotation(ctx, latestNamespace, currentNamespaceClass.Name); err != nil {
			return err
		}

		// Trigger reconciliation for the old class to update its status
		r.triggerReconcile(ctx, previousClass)
	} else if currentClass == "" {
		// No current class recorded, but original class exists - initialize current class
		if err := r.updateCurrentClassAnnotation(ctx, latestNamespace, currentNamespaceClass.Name); err != nil {
			return err
		}
	} else {
		logger.Info("No class switch detected",
			"namespace", latestNamespace.Name,
			"class", currentNamespaceClass.Name)
	}

	// Reconcile the NamespaceClass resources
	if err := r.reconcileNamespaceClassResources(ctx, latestNamespace, currentNamespaceClass); err != nil {
		logger.Error(err, "failed to reconcile NamespaceClass resources", "namespace", latestNamespace.Name)
		return err
	}

	// Update status to track lastAppliedClass for class switching
	if previousClass != "" && previousClass != currentNamespaceClass.Name {
		if err := r.updateClassSwitchStatus(ctx, latestNamespace, currentNamespaceClass.Name, previousClass); err != nil {
			logger.Error(err, "Failed to update class switch status",
				"namespace", latestNamespace.Name,
				"currentClass", currentNamespaceClass.Name,
				"previousClass", previousClass)
		}
	}

	return nil
}

// triggerReconcile queues a reconciliation for the specified NamespaceClass
func (r *NamespaceClassReconciler) triggerReconcile(ctx context.Context, className string) {
	logger := logf.FromContext(ctx)

	if className == "" {
		return
	}

	// Create a request to reconcile the class
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: className,
		},
	}

	logger.Info("Triggering reconciliation for class", "class", className)

	go func() {
		// Use a small delay to let current reconciliation finish
		time.Sleep(100 * time.Millisecond)
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			logger.Error(err, "Failed to trigger reconciliation for class", "class", className)
		}
	}()
}

// getNamespace returns the Namespace object for the given namespace name.
func (r *NamespaceClassReconciler) getNamespace(ctx context.Context, namespaceName string) (*corev1.Namespace, error) {
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace); err != nil {
		return nil, err
	}
	return namespace, nil
}

// getNamespaceClass returns the NamespaceClass object for the given namespace.
// Returns the NamespaceClass on success or an error if it can't be retrieved.
func (r *NamespaceClassReconciler) getNamespaceClass(ctx context.Context, namespace *corev1.Namespace) (*v1alpha1.NamespaceClass, error) {
	logger := logf.FromContext(ctx)

	// Get the NamespaceClass name from the namespace label
	namespaceLabelValue := namespace.Labels[NamespaceClassLabel]
	if namespaceLabelValue == "" {
		return nil, nil
	}

	// Get the NamespaceClass
	namespaceClass := &v1alpha1.NamespaceClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespaceLabelValue}, namespaceClass); err != nil {
		return nil, err
	}

	logger.V(1).Info("Found NamespaceClass for namespace",
		"namespace", namespace.Name,
		"namespaceClass", namespaceClass.Name)

	return namespaceClass, nil
}

// reconcileNamespaceClassResources reconciles the resources for a NamespaceClass.
func (r *NamespaceClassReconciler) reconcileNamespaceClassResources(ctx context.Context, namespace *corev1.Namespace, namespaceClass *v1alpha1.NamespaceClass) error {
	logger := logf.FromContext(ctx)
	logger.Info("Reconciling resources for namespace",
		"namespace", namespace.Name,
		"namespaceClass", namespaceClass.Name)

	// Double-check if namespace is being deleted - don't try to apply resources
	if namespace.DeletionTimestamp != nil {
		logger.V(1).Info("Namespace is being deleted, skipping resource application",
			"namespace", namespace.Name,
			"namespaceClass", namespaceClass.Name)
		return nil
	}

	// Get the latest namespace state - this is important to have the most up-to-date
	// labels and annotations, especially the LastAppliedClassAnnotation
	latestNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace.Name}, latestNamespace); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Namespace no longer exists, skipping resource application",
				"namespace", namespace.Name)
			return nil
		}
		return fmt.Errorf("failed to get latest namespace state: %w", err)
	}

	// Check if namespace is being deleted
	if latestNamespace.DeletionTimestamp != nil {
		logger.V(1).Info("Namespace is now being deleted, skipping resources",
			"namespace", namespace.Name)
		return nil
	}

	// Get the resources from the namespaceClass
	resources := namespaceClass.Spec.Resources
	if len(resources) == 0 {
		logger.Info("No resources defined in namespaceClass",
			"namespace", latestNamespace.Name,
			"namespaceClass", namespaceClass.Name)
		return nil
	}

	// We need to check to see if resources have been applied already, if so, we should re-reconcile them to make sure they are still correct
	currentResources, err := r.getCurrentNamespaceClassResources(ctx, latestNamespace, namespaceClass)
	if err != nil {
		logger.Error(err, "failed to get current namespaceClass resources", "namespace", latestNamespace.Name)
		return err
	}

	logger.V(1).Info("Current resources found",
		"namespace", latestNamespace.Name,
		"count", len(currentResources),
		"resources", currentResources)

	// Apply each resource in the namespaceClass to the namespace
	for i, resource := range resources {
		resourceIndex := i + 1
		logger.V(1).Info("Applying resource to namespace",
			"namespace", latestNamespace.Name,
			"resourceIndex", resourceIndex,
			"totalResources", len(resources))

		// Check again before each resource application
		if err := r.Get(ctx, types.NamespacedName{Name: latestNamespace.Name}, latestNamespace); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("Namespace no longer exists during resource application",
					"namespace", latestNamespace.Name)
				return nil
			}
			return fmt.Errorf("failed to get latest namespace state: %w", err)
		}

		if latestNamespace.DeletionTimestamp != nil {
			logger.V(1).Info("Namespace is now being deleted, skipping remaining resources",
				"namespace", latestNamespace.Name)
			return nil
		}

		if err := r.applyResourceToNamespace(ctx, latestNamespace, namespaceClass, resource); err != nil {
			logger.Error(err, "failed to apply resource to namespace",
				"namespace", latestNamespace.Name,
				"resourceIndex", resourceIndex)
			return err
		}
	}

	logger.Info("Successfully applied resources to namespace",
		"namespace", latestNamespace.Name,
		"namespaceClass", namespaceClass.Name,
		"resourceCount", len(resources))
	return nil
}

// setClassAnnotations sets both the original and current class annotations on a namespace.
// This is used during the first-time setup of a namespace with a class.
func (r *NamespaceClassReconciler) setClassAnnotations(ctx context.Context, namespace *corev1.Namespace, originalClass, currentClass string) error {
	logger := logf.FromContext(ctx)

	// Use mutex to prevent multiple controllers from updating the annotation concurrently
	namespaceLock, _ := r.namespaceMutex.LoadOrStore(namespace.Name, &sync.Mutex{})
	mutex := namespaceLock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	logger.Info("Setting class annotations",
		"namespace", namespace.Name,
		"originalClass", originalClass,
		"currentClass", currentClass)

	// Get the latest namespace state to prevent update conflicts
	latestNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace.Name}, latestNamespace); err != nil {
		return fmt.Errorf("failed to get latest namespace state: %w", err)
	}

	// Initialize annotations map if it doesn't exist
	if latestNamespace.Annotations == nil {
		latestNamespace.Annotations = make(map[string]string)
	}

	// Set both annotations
	latestNamespace.Annotations[LastAppliedClassAnnotation] = originalClass
	latestNamespace.Annotations[CurrentClassAnnotation] = currentClass

	// Update the namespace in the cluster
	if err := r.Update(ctx, latestNamespace); err != nil {
		logger.Error(err, "Failed to set class annotations",
			"namespace", latestNamespace.Name,
			"originalClass", originalClass,
			"currentClass", currentClass)
		return err
	}

	logger.Info("Successfully set class annotations",
		"namespace", latestNamespace.Name,
		"originalClass", originalClass,
		"currentClass", currentClass)
	return nil
}

// updateCurrentClassAnnotation updates the current class annotation on a namespace.
// This is used when switching from one class to another, while also updating the last applied class.
func (r *NamespaceClassReconciler) updateCurrentClassAnnotation(ctx context.Context, namespace *corev1.Namespace, currentClass string) error {
	logger := logf.FromContext(ctx)

	// Use mutex to prevent multiple controllers from updating the annotation concurrently
	namespaceLock, _ := r.namespaceMutex.LoadOrStore(namespace.Name, &sync.Mutex{})
	mutex := namespaceLock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	logger.Info("Updating class annotations",
		"namespace", namespace.Name,
		"newCurrentClass", currentClass)

	// Get the latest namespace state to prevent update conflicts
	latestNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace.Name}, latestNamespace); err != nil {
		return fmt.Errorf("failed to get latest namespace state: %w", err)
	}

	// Get existing current class before it changes - this will be the new last-applied-class
	existingCurrentClass := ""
	if latestNamespace.Annotations != nil {
		existingCurrentClass = latestNamespace.Annotations[CurrentClassAnnotation]
	}

	// Initialize annotations map if it doesn't exist
	if latestNamespace.Annotations == nil {
		latestNamespace.Annotations = make(map[string]string)
	}

	// Only update last-applied-class if:
	// 1. There's an existing current class (not first-time setup)
	// 2. It's different from the new class (actual switch)
	if existingCurrentClass != "" && existingCurrentClass != currentClass {
		// Update last-applied-class annotation to the previous current class
		latestNamespace.Annotations[LastAppliedClassAnnotation] = existingCurrentClass
		logger.Info("Updating last-applied-class annotation",
			"namespace", latestNamespace.Name,
			"newLastAppliedClass", existingCurrentClass)
	}

	// Update the current class annotation
	latestNamespace.Annotations[CurrentClassAnnotation] = currentClass

	// Update the namespace in the cluster
	if err := r.Update(ctx, latestNamespace); err != nil {
		logger.Error(err, "Failed to update class annotations",
			"namespace", latestNamespace.Name,
			"previousCurrentClass", existingCurrentClass,
			"newCurrentClass", currentClass)
		return err
	}

	logger.Info("Successfully updated class annotations",
		"namespace", latestNamespace.Name,
		"previousCurrentClass", existingCurrentClass,
		"newCurrentClass", currentClass)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create a predicate that filters resources with our owner label
	ownedResourcePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetLabels()[NamespaceClassOwner] != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetLabels()[NamespaceClassOwner] != "" ||
				e.ObjectOld.GetLabels()[NamespaceClassOwner] != ""
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object.GetLabels()[NamespaceClassOwner] != ""
		},
	}

	// Add a helper to enqueue reconcile requests for the owning NamespaceClass
	enqueueForOwningNamespaceClass := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			logger := logf.FromContext(ctx)

			// Get the owner label
			ownerName := obj.GetLabels()[NamespaceClassOwner]
			if ownerName == "" {
				return nil
			}

			// Get the resource's namespace
			resourceNamespace := obj.GetNamespace()
			if resourceNamespace == "" {
				logger.Info("Resource with owner label has no namespace, skipping",
					"resource", obj.GetName(),
					"namespaceClass", ownerName)
				return nil
			}

			// Get the resource's kind
			gvk := obj.GetObjectKind().GroupVersionKind()
			kind := gvk.Kind
			// If kind is empty, try to get it from object type name
			if kind == "" {
				// Extract type name from object
				typeName := fmt.Sprintf("%T", obj)
				// Log the full type for debugging
				logger.V(1).Info("Getting kind from type name", "typeName", typeName)
				// Generally, the last part of the type name will be the kind
				parts := strings.Split(typeName, ".")
				if len(parts) > 0 {
					kind = parts[len(parts)-1]
				}
			}

			logger.Info("Resource with owner label changed, reconciling namespace",
				"resource", obj.GetName(),
				"namespace", resourceNamespace,
				"kind", kind,
				"namespaceClass", ownerName)

			// Return a request to reconcile the namespace containing the resource
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name: resourceNamespace,
					},
				},
			}
		})

	// Set up dynamic resource watches
	if err := r.setupResourceWatchers(mgr, ownedResourcePredicate, enqueueForOwningNamespaceClass); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.NamespaceClass{}).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueNamespaceWithClassFilter),
			builder.WithPredicates(predicate.Funcs{
				// Process namespaces that have the namespaceclass.akuity.io/name label or relevant annotations
				CreateFunc: func(e event.CreateEvent) bool {
					return (e.Object.GetLabels()[NamespaceClassLabel] != "" ||
						e.Object.GetAnnotations()[CurrentClassAnnotation] != "") &&
						e.Object.GetDeletionTimestamp() == nil
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					// Skip if the new object is being deleted
					if e.ObjectNew.GetDeletionTimestamp() != nil {
						return false
					}

					// Watch for label changes
					newLabel := e.ObjectNew.GetLabels()[NamespaceClassLabel]
					oldLabel := e.ObjectOld.GetLabels()[NamespaceClassLabel]

					// Watch for annotation changes
					newAnnotation := e.ObjectNew.GetAnnotations()[CurrentClassAnnotation]
					oldAnnotation := e.ObjectOld.GetAnnotations()[CurrentClassAnnotation]

					// Either value is non-empty AND there was a change
					hasLabelChange := (newLabel != oldLabel) || (newLabel != "" || oldLabel != "")
					hasAnnotationChange := (newAnnotation != oldAnnotation) || (newAnnotation != "" || oldAnnotation != "")
					return hasLabelChange || hasAnnotationChange
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return e.Object.GetLabels()[NamespaceClassLabel] != "" ||
						e.Object.GetAnnotations()[CurrentClassAnnotation] != ""
				},
			}),
		).
		Watches(
			&v1alpha1.NamespaceClass{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				logger := logf.FromContext(ctx)
				logger.Info("NamespaceClass changed, reconciling namespaces", "namespaceclass", obj.GetName())

				// Use a label selector to only get namespaces with the matching label
				nsList := &corev1.NamespaceList{}
				if err := r.List(ctx, nsList, client.MatchingLabels{
					NamespaceClassLabel: obj.GetName(),
				}); err != nil {
					logger.Error(err, "failed to list namespaces")
					return nil
				}

				var requests []reconcile.Request
				for _, namespace := range nsList.Items {
					logger.Info("Reconciling namespace", "namespace", namespace.Name)
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name: namespace.Name,
						},
					})
				}
				return requests
			}),
		).
		Named("namespaceclass").
		Complete(r)
}

// enqueueNamespaceWithClassFilter is a predicate that triggers reconciliation for namespaces with a namespaceClass label
// and for any previously associated NamespaceClass when the label changes
func (r *NamespaceClassReconciler) enqueueNamespaceWithClassFilter(ctx context.Context, o client.Object) []reconcile.Request {
	logger := logf.FromContext(ctx)
	namespace, ok := o.(*corev1.Namespace)
	if !ok {
		logger.Error(fmt.Errorf("expected a Namespace but got a %T", o), "unable to reconcile namespace")
		return nil
	}

	// Skip reconciling if the namespace is being deleted to avoid creating resources
	// in a namespace that's being terminated
	if namespace.DeletionTimestamp != nil {
		logger.Info("Namespace is being deleted, skipping reconciliation",
			"namespace", namespace.Name)
		return nil
	}

	var requests []reconcile.Request

	// Get the current namespace class name from the label
	currentClassName := ""
	if namespace.Labels != nil {
		currentClassName = namespace.Labels[NamespaceClassLabel]
	}

	// Check for the current annotation to detect class switches
	previousClassName := ""
	if namespace.Annotations != nil {
		previousClassName = namespace.Annotations[CurrentClassAnnotation]
	}

	// We need to trigger reconciliation in these cases:
	// 1. For the namespace itself - always
	// 2. For the new class - if it exists
	// 3. For the old class - if it's different from the new class (class switch)

	// Always add the namespace itself to be reconciled
	if currentClassName != "" || previousClassName != "" {
		logger.Info("Adding namespace for reconciliation",
			"namespace", namespace.Name,
			"currentClass", currentClassName,
			"previousClass", previousClassName)

		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: namespace.Name,
			},
		})
	}

	// If we're switching classes, also trigger reconciliation for both the old and new classes
	if previousClassName != "" && currentClassName != previousClassName && previousClassName != currentClassName {
		logger.Info("Detected class switch, reconciling both classes",
			"namespace", namespace.Name,
			"previousClass", previousClassName,
			"currentClass", currentClassName)

		// Add the previous class for reconciliation
		if previousClassName != "" {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: previousClassName,
				},
			})
		}
	}

	// Also add the current class (if different from previous)
	if currentClassName != "" && currentClassName != previousClassName {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: currentClassName,
			},
		})
	}

	return requests
}

// cleanupAllNamespaceClassResources removes all resources labeled with a specific NamespaceClass.
// This is called when the previous NamespaceClass no longer exists.
func (r *NamespaceClassReconciler) cleanupAllNamespaceClassResources(ctx context.Context, namespace *corev1.Namespace, className string) error {
	logger := logf.FromContext(ctx)
	logger.Info("Cleaning up all resources for class",
		"namespace", namespace.Name,
		"class", className)

	// List all resources with the owner label for this class
	resources := r.listResourcesWithLabel(ctx, namespace.Name, NamespaceClassOwner, className)

	logger.Info("Found resources to delete",
		"namespace", namespace.Name,
		"class", className,
		"count", len(resources))

	// Delete each found resource
	for _, resource := range resources {
		gvk := resource.GetObjectKind().GroupVersionKind()
		name := resource.GetName()

		logger.Info("Deleting resource",
			"namespace", namespace.Name,
			"class", className,
			"kind", gvk.Kind,
			"name", name)

		// Delete the resource
		if err := r.Delete(ctx, resource); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "Failed to delete resource",
				"namespace", namespace.Name,
				"class", className,
				"kind", gvk.Kind,
				"name", name)
		}
	}

	logger.Info("Successfully cleaned up all resources for class",
		"namespace", namespace.Name,
		"class", className,
		"count", len(resources))
	return nil
}
