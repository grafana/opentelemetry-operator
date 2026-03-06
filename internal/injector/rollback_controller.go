// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clockutil "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

const (
	defaultGraceTime       = 5 * time.Minute
	defaultStabilityWindow = 1 * time.Hour

	reasonCrashLoopBackOff = "CrashLoopBackOff"
	reasonImagePullBackOff = "ImagePullBackOff"
)

// RollbackReconciler watches pods for crash loops and manages rollback state
// on Instrumentation CRs.
type RollbackReconciler struct {
	client.Client
	log     logr.Logger
	clock   clockutil.Clock
	metrics *v2alpha1.InstrumentationMetrics
}

// NewRollbackReconciler creates a new RollbackReconciler.
// metrics may be nil, in which case metric reporting is skipped.
func NewRollbackReconciler(c client.Client, _ *runtime.Scheme, log logr.Logger, metrics *v2alpha1.InstrumentationMetrics) *RollbackReconciler {
	return &RollbackReconciler{
		Client:  c,
		log:     log,
		clock:   clockutil.RealClock{},
		metrics: metrics,
	}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;patch

// SetupWithManager registers the controller.
func (r *RollbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("instrumentation-rollback").
		For(&v2alpha1.Instrumentation{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToInstrumentations),
		).
		Complete(r)
}

// podToInstrumentations maps a pod event to the Instrumentation CRs that might apply to it.
func (r *RollbackReconciler) podToInstrumentations(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	if !isRelevantPod(*pod) {
		return nil
	}

	var list v2alpha1.InstrumentationList
	if err := r.List(ctx, &list); err != nil {
		r.log.Error(err, "failed to list Instrumentation CRs in pod mapper")
		return nil
	}

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: pod.Namespace}}
	var requests []reconcile.Request
	for _, inst := range list.Items {
		if hasMatchingRule(&inst, ns.Name, *pod) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&inst),
			})
		}
	}
	return requests
}

// isRelevantPod returns true if the pod has our LD_PRELOAD or is in a crash state.
func isRelevantPod(pod corev1.Pod) bool {
	return hasOurLDPreload(pod) || podCrashReason(pod) != ""
}

type resolvedRollbackConfig struct {
	enabled         bool
	graceTime       time.Duration
	stabilityWindow time.Duration
}

func resolveRollbackConfig(cfg *v2alpha1.RollbackConfig) resolvedRollbackConfig {
	rc := resolvedRollbackConfig{
		enabled:         true,
		graceTime:       defaultGraceTime,
		stabilityWindow: defaultStabilityWindow,
	}
	if cfg == nil {
		return rc
	}
	if cfg.Enabled != nil {
		rc.enabled = *cfg.Enabled
	}
	if cfg.GraceTime != nil {
		rc.graceTime = cfg.GraceTime.Duration
	}
	if cfg.StabilityWindow != nil {
		rc.stabilityWindow = cfg.StabilityWindow.Duration
	}
	return rc
}

// Reconcile processes a single Instrumentation CR: builds workload inventory,
// detects crashes, and triggers rollbacks.
func (r *RollbackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("instrumentation", req.Name)

	var inst v2alpha1.Instrumentation
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if inst.DeletionTimestamp != nil {
		if r.metrics != nil {
			r.metrics.DeleteCR(inst.Name)
		}
		return ctrl.Result{}, nil
	}

	rc := resolveRollbackConfig(inst.Spec.Defaults.Rollback)
	if !rc.enabled {
		log.V(1).Info("rollback disabled, skipping")
		return ctrl.Result{}, nil
	}

	// Build current workload inventory from live pods.
	// The pod cache is shared between buildWorkloadInventory and checkCrashState
	// to avoid redundant API calls for the same namespace.
	cache := make(podCache)
	inventory, err := r.buildWorkloadInventory(ctx, &inst, cache)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Merge with existing status.
	now := metav1.NewTime(r.clock.Now())
	entries := mergeInventory(inst.Status.InstrumentedWorkloads, inventory, inst.Generation, now)

	// Check each entry for recovery and crash state.
	var requeueAfter time.Duration
	for i := range entries {
		entry := &entries[i]

		// Recovery: CR generation changed after rollback → clear rollback.
		if entry.Rollback != nil && inst.Generation > entry.CRGeneration {
			log.Info("CR generation changed, clearing rollback", "workload", entry.WorkloadRef)
			entry.Rollback = nil
			entry.CRGeneration = inst.Generation
			entry.InstrumentedAt = now
		}

		if entry.Rollback != nil {
			continue
		}

		crashReason := r.checkCrashState(ctx, cache, entry.WorkloadRef)
		if crashReason == "" {
			continue
		}

		timeSinceInstrumented := r.clock.Since(entry.InstrumentedAt.Time)

		// Past stability window → ignore.
		if timeSinceInstrumented > rc.stabilityWindow {
			log.V(1).Info("crash outside stability window, ignoring",
				"workload", entry.WorkloadRef, "reason", crashReason)
			continue
		}

		// Within grace period → requeue.
		if timeSinceInstrumented < rc.graceTime {
			remaining := rc.graceTime - timeSinceInstrumented
			log.V(1).Info("crash within grace period, will recheck",
				"workload", entry.WorkloadRef, "reason", crashReason, "remaining", remaining)
			if requeueAfter == 0 || remaining < requeueAfter {
				requeueAfter = remaining
			}
			continue
		}

		// Trigger rollback.
		log.Info("triggering rollback",
			"workload", entry.WorkloadRef, "reason", crashReason)
		entry.Rollback = &v2alpha1.RollbackInfo{
			Reason:       crashReason,
			RolledBackAt: now,
		}
		if err := r.patchRestartAnnotation(ctx, entry.WorkloadRef); err != nil {
			log.Error(err, "failed to patch restart annotation", "workload", entry.WorkloadRef)
		}
	}

	// Update status.
	inst.Status.InstrumentedWorkloads = entries
	if r.metrics != nil {
		r.metrics.UpdateCR(inst.Name, computeStatusCounts(&inst, cache, entries))
	}
	if err := r.Status().Update(ctx, &inst); err != nil {
		return ctrl.Result{}, err
	}

	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// podCache caches pod lists by namespace to avoid redundant API calls.
type podCache map[string][]corev1.Pod

func (r *RollbackReconciler) listPods(ctx context.Context, cache podCache, ns string) ([]corev1.Pod, error) {
	if pods, ok := cache[ns]; ok {
		return pods, nil
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing pods in namespace %s: %w", ns, err)
	}
	cache[ns] = podList.Items
	return podList.Items, nil
}

// buildWorkloadInventory discovers all pods instrumented by this CR and groups them by workload.
// It populates the provided podCache so callers can reuse the fetched pods.
func (r *RollbackReconciler) buildWorkloadInventory(ctx context.Context, inst *v2alpha1.Instrumentation, cache podCache) (map[v2alpha1.WorkloadReference]string, error) {
	inventory := make(map[v2alpha1.WorkloadReference]string)

	for _, rule := range inst.Spec.Rules {
		namespaces, err := resolveNamespaces(ctx, r.Client, rule.Selector.Namespaces)
		if err != nil {
			return nil, err
		}

		for _, ns := range namespaces {
			pods, err := r.listPods(ctx, cache, ns)
			if err != nil {
				return nil, err
			}

			for _, pod := range pods {
				if !hasOurLDPreload(pod) {
					continue
				}
				if !matchesPodLabels(rule.Selector, pod.Labels) {
					continue
				}
				ref := resolveWorkloadRef(pod)
				if ref == nil {
					continue
				}
				if _, exists := inventory[*ref]; !exists {
					inventory[*ref] = rule.Name
				}
			}
		}
	}

	return inventory, nil
}

// podCrashReason returns the crash reason if any container is in a crash state, or "".
func podCrashReason(pod corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			if reason == reasonCrashLoopBackOff || reason == reasonImagePullBackOff {
				return reason
			}
		}
	}
	return ""
}

// hasOurLDPreload returns true if any container has our specific LD_PRELOAD value.
func hasOurLDPreload(pod corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == envLDPreload && e.Value == ldPreloadPath {
				return true
			}
		}
	}
	return false
}

// mergeInventory merges the current live inventory with existing status entries.
// Preserves instrumentedAt and rollback info from existing entries; adds new workloads;
// drops workloads no longer present.
func mergeInventory(existing []v2alpha1.InstrumentedWorkload, current map[v2alpha1.WorkloadReference]string, generation int64, now metav1.Time) []v2alpha1.InstrumentedWorkload {
	existingMap := make(map[v2alpha1.WorkloadReference]*v2alpha1.InstrumentedWorkload, len(existing))
	for i := range existing {
		existingMap[existing[i].WorkloadRef] = &existing[i]
	}

	var result []v2alpha1.InstrumentedWorkload
	for ref, ruleName := range current {
		if prev, ok := existingMap[ref]; ok {
			// Preserve existing entry (keeps instrumentedAt, rollback, crGeneration).
			entry := *prev
			entry.RuleName = ruleName
			result = append(result, entry)
		} else {
			// New workload.
			result = append(result, v2alpha1.InstrumentedWorkload{
				WorkloadRef:    ref,
				RuleName:       ruleName,
				InstrumentedAt: now,
				CRGeneration:   generation,
			})
		}
	}

	// Sort deterministically by kind/namespace/name.
	sort.Slice(result, func(i, j int) bool {
		ri, rj := result[i].WorkloadRef, result[j].WorkloadRef
		if ri.Kind != rj.Kind {
			return ri.Kind < rj.Kind
		}
		if ri.Namespace != rj.Namespace {
			return ri.Namespace < rj.Namespace
		}
		return ri.Name < rj.Name
	})

	return result
}

// checkCrashState checks if any pod of the given workload is in a crash state.
// Returns the crash reason or empty string if healthy.
func (r *RollbackReconciler) checkCrashState(ctx context.Context, cache podCache, ref v2alpha1.WorkloadReference) string {
	pods, err := r.listPods(ctx, cache, ref.Namespace)
	if err != nil {
		r.log.Error(err, "failed to list pods for crash check", "workload", ref)
		return ""
	}

	for _, pod := range pods {
		wRef := resolveWorkloadRef(pod)
		if wRef == nil || *wRef != ref {
			continue
		}
		if reason := podCrashReason(pod); reason != "" {
			return reason
		}
	}
	return ""
}

// patchRestartAnnotation patches the workload's pod template with a restartedAt annotation,
// triggering a rolling restart.
func (r *RollbackReconciler) patchRestartAnnotation(ctx context.Context, ref v2alpha1.WorkloadReference) error {
	patch := fmt.Appendf(nil,
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
		r.clock.Now().Format(time.RFC3339),
	)

	var obj client.Object
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}

	switch ref.Kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "StatefulSet":
		obj = &appsv1.StatefulSet{}
	case "DaemonSet":
		obj = &appsv1.DaemonSet{}
	default:
		return fmt.Errorf("unsupported workload kind for restart: %s", ref.Kind)
	}

	obj.SetName(key.Name)
	obj.SetNamespace(key.Namespace)
	return r.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

// shouldSkipForRollback checks if a pod's workload has been rolled back and the
// CR generation hasn't changed since. Returns true if injection should be skipped.
func shouldSkipForRollback(inst *v2alpha1.Instrumentation, pod corev1.Pod) bool {
	ref := resolveWorkloadRef(pod)
	if ref == nil {
		return false
	}

	for _, entry := range inst.Status.InstrumentedWorkloads {
		if entry.WorkloadRef == *ref && entry.Rollback != nil && inst.Generation <= entry.CRGeneration {
			return true
		}
	}
	return false
}
