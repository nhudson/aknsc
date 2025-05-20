package controller

import (
	"context"
	"fmt"

	v1alpha1 "github.com/nhudson/aknsc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// updateNamespaceClassStatus orchestrates the status update for a NamespaceClass
func (r *NamespaceClassReconciler) updateNamespaceClassStatus(ctx context.Context, namespaceClass *v1alpha1.NamespaceClass) error {
	logger := log.FromContext(ctx).WithValues("namespaceClass", namespaceClass.Name)
	logger.Info("Updating NamespaceClass status")

	// Get a fresh copy of the namespaceClass
	freshClass, err := r.getFreshNamespaceClass(ctx, namespaceClass.Name)
	if err != nil {
		return err
	}

	// Find namespaces using this class
	namespaces, err := r.getNamespacesForClass(ctx, namespaceClass.Name)
	if err != nil {
		return err
	}

	// Build status entries for namespaces
	statusEntries := r.buildStatusEntries(ctx, freshClass, namespaces)

	// Apply the status update
	return r.applyStatusUpdate(ctx, freshClass, statusEntries)
}

// getFreshNamespaceClass gets a fresh copy of the NamespaceClass to avoid update conflicts
func (r *NamespaceClassReconciler) getFreshNamespaceClass(ctx context.Context, name string) (*v1alpha1.NamespaceClass, error) {
	freshClass := &v1alpha1.NamespaceClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, freshClass); err != nil {
		return nil, fmt.Errorf("failed to get fresh copy of NamespaceClass: %w", err)
	}
	return freshClass, nil
}

// getNamespacesForClass lists all namespaces with a specific class label
func (r *NamespaceClassReconciler) getNamespacesForClass(ctx context.Context, className string) (*corev1.NamespaceList, error) {
	namespaceList := &corev1.NamespaceList{}
	if err := r.List(ctx, namespaceList, client.MatchingLabels{
		NamespaceClassLabel: className,
	}); err != nil {
		return nil, fmt.Errorf("failed to list namespaces with class label: %w", err)
	}
	return namespaceList, nil
}

// buildStatusEntries creates status entries for namespaces using this class
// It preserves existing lastAppliedClass values when available
func (r *NamespaceClassReconciler) buildStatusEntries(
	ctx context.Context,
	class *v1alpha1.NamespaceClass,
	namespaces *corev1.NamespaceList,
) []v1alpha1.NamespaceClassNamespaceStatus {
	logger := log.FromContext(ctx).WithValues("namespaceClass", class.Name)

	// Create lookup map for existing status entries
	existingStatusMap := makeStatusLookupMap(class.Status.Namespaces)

	// Pre-allocate slice with capacity based on number of namespaces
	result := make([]v1alpha1.NamespaceClassNamespaceStatus, 0, len(namespaces.Items))

	// Process each namespace
	for _, ns := range namespaces.Items {
		// Only include namespaces actually using this class
		if ns.Labels[NamespaceClassLabel] != class.Name {
			continue
		}

		// Find or create status entry
		entry := createNamespaceStatusEntry(ns.Name, class.Name, existingStatusMap)

		// Add to result
		result = append(result, entry)
		logger.V(1).Info("Added namespace to status",
			"namespace", entry.Name,
			"currentClass", entry.CurrentClass,
			"lastAppliedClass", entry.LastAppliedClass)
	}

	return result
}

// makeStatusLookupMap creates a map of namespace name to status for quick lookups
func makeStatusLookupMap(statuses []v1alpha1.NamespaceClassNamespaceStatus) map[string]v1alpha1.NamespaceClassNamespaceStatus {
	result := make(map[string]v1alpha1.NamespaceClassNamespaceStatus, len(statuses))
	for _, status := range statuses {
		result[status.Name] = status
	}
	return result
}

// createNamespaceStatusEntry creates a status entry for a namespace, preserving existing lastAppliedClass if available
func createNamespaceStatusEntry(
	namespaceName string,
	className string,
	existingStatusMap map[string]v1alpha1.NamespaceClassNamespaceStatus,
) v1alpha1.NamespaceClassNamespaceStatus {
	entry := v1alpha1.NamespaceClassNamespaceStatus{
		Name:             namespaceName,
		CurrentClass:     className,
		LastAppliedClass: "",
		ResourceStatus:   "Applied",
	}

	// If we have an existing status entry for this namespace, preserve its lastAppliedClass
	if existing, exists := existingStatusMap[namespaceName]; exists && existing.LastAppliedClass != "" {
		entry.LastAppliedClass = existing.LastAppliedClass
	}

	return entry
}

// applyStatusUpdate updates the status with the new entries and conditions
func (r *NamespaceClassReconciler) applyStatusUpdate(
	ctx context.Context,
	class *v1alpha1.NamespaceClass,
	namespaceStatuses []v1alpha1.NamespaceClassNamespaceStatus,
) error {
	logger := log.FromContext(ctx).WithValues("namespaceClass", class.Name)

	// Create patch
	patch := client.MergeFrom(class.DeepCopy())

	// Update the observed generation
	class.Status.ObservedGeneration = class.Generation

	// Update ready condition
	updateReadyCondition(class)

	// Update namespaces list
	class.Status.Namespaces = namespaceStatuses

	// Apply the status update
	if err := r.Status().Patch(ctx, class, patch); err != nil {
		return fmt.Errorf("failed to update NamespaceClass status: %w", err)
	}

	logger.Info("Successfully updated NamespaceClass status", "namespaceCount", len(namespaceStatuses))
	return nil
}

// updateReadyCondition sets the Ready condition on the NamespaceClass
func updateReadyCondition(class *v1alpha1.NamespaceClass) {
	meta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ResourcesApplied",
		Message:            "All resources applied successfully",
		LastTransitionTime: metav1.Now(),
	})
}

// updateClassSwitchStatus updates the status of a NamespaceClass when a namespace switches to or from it
// It ensures the lastAppliedClass field properly tracks the previous class
func (r *NamespaceClassReconciler) updateClassSwitchStatus(
	ctx context.Context,
	namespace *corev1.Namespace,
	currentClass string,
	previousClass string,
) error {
	if previousClass == "" || previousClass == currentClass {
		return nil
	}

	logger := log.FromContext(ctx).WithValues(
		"namespace", namespace.Name,
		"currentClass", currentClass,
		"previousClass", previousClass,
	)
	logger.Info("Updating NamespaceClass status for class switch")

	// Get a fresh copy of the current class
	freshClass, err := r.getFreshNamespaceClass(ctx, currentClass)
	if err != nil {
		logger.Error(err, "Failed to get fresh copy of current NamespaceClass")
		return err
	}

	// Create a lookup map of existing entries
	existingEntries := makeStatusLookupMap(freshClass.Status.Namespaces)

	// Find or create the status entry for this namespace
	updatedStatuses := updateStatusForClassSwitch(
		freshClass.Status.Namespaces,
		existingEntries,
		namespace.Name,
		currentClass,
		previousClass,
	)

	return r.applyStatusUpdate(ctx, freshClass, updatedStatuses)
}

// updateStatusForClassSwitch updates the status entries list with the proper lastAppliedClass
// for a namespace that has switched classes
func updateStatusForClassSwitch(
	currentStatuses []v1alpha1.NamespaceClassNamespaceStatus,
	statusMap map[string]v1alpha1.NamespaceClassNamespaceStatus,
	namespaceName string,
	currentClass string,
	previousClass string,
) []v1alpha1.NamespaceClassNamespaceStatus {
	existingStatus, exists := statusMap[namespaceName]

	// Create the updated status list
	var result []v1alpha1.NamespaceClassNamespaceStatus

	if exists {
		// Update the existing entry
		existingStatus.LastAppliedClass = previousClass

		// Build a new status list with the updated entry
		result = make([]v1alpha1.NamespaceClassNamespaceStatus, 0, len(currentStatuses))
		for _, status := range currentStatuses {
			if status.Name == namespaceName {
				result = append(result, existingStatus)
			} else {
				result = append(result, status)
			}
		}
	} else {
		newStatus := v1alpha1.NamespaceClassNamespaceStatus{
			Name:             namespaceName,
			CurrentClass:     currentClass,
			LastAppliedClass: previousClass,
			ResourceStatus:   "Applied",
		}

		result = make([]v1alpha1.NamespaceClassNamespaceStatus, len(currentStatuses)+1)
		copy(result, currentStatuses)
		result[len(currentStatuses)] = newStatus
	}

	return result
}
