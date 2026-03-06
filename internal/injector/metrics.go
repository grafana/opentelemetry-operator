// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

// computeStatusCounts classifies every pod in the cache by instrumentation status
// and returns measurements ready to pass to InstrumentationMetrics.UpdateCR.
//
// Classification logic per pod (first match wins):
//  1. Workload has active rollback in entries → rolled_back
//  2. Pod has our LD_PRELOAD → instrumented
//  3. Pod matches a skip-mode rule → skipped
//  4. Pod matches a non-skip rule → pending_restart
//  5. No rule match → unmatched
//
// Container-name selectors are intentionally ignored for pod-level classification.
// The scan is bounded to the namespaces already in the podCache.
func computeStatusCounts(
	inst *v2alpha1.Instrumentation,
	cache podCache,
	entries []v2alpha1.InstrumentedWorkload,
) []v2alpha1.Measurement {
	// Build rollback lookup for O(1) lookup per pod.
	entryByRef := make(map[v2alpha1.WorkloadReference]*v2alpha1.InstrumentedWorkload, len(entries))
	for i := range entries {
		entryByRef[entries[i].WorkloadRef] = &entries[i]
	}

	type key struct {
		namespace    string
		workloadKind string
		workloadName string
		cr           string
		rule         string
		status       string
	}
	counts := make(map[key]int64)

	for ns, pods := range cache {
		for _, pod := range pods {
			cr, rule, status := classifyPod(pod, ns, inst, entryByRef)

			var wKind, wName string
			ref := resolveWorkloadRef(pod)
			if ref != nil {
				wKind = ref.Kind
				wName = ref.Name
			} else {
				wKind = "Pod"
				wName = pod.Name
			}

			counts[key{
				namespace:    ns,
				workloadKind: wKind,
				workloadName: wName,
				cr:           cr,
				rule:         rule,
				status:       status,
			}]++
		}
	}

	result := make([]v2alpha1.Measurement, 0, len(counts))
	for k, count := range counts {
		result = append(result, v2alpha1.Measurement{
			Count: count,
			Attrs: attribute.NewSet(
				attribute.String(v2alpha1.AttrNamespace, k.namespace),
				attribute.String(v2alpha1.AttrWorkloadKind, k.workloadKind),
				attribute.String(v2alpha1.AttrWorkloadName, k.workloadName),
				attribute.String(v2alpha1.AttrCR, k.cr),
				attribute.String(v2alpha1.AttrRule, k.rule),
				attribute.String(v2alpha1.AttrStatus, k.status),
			),
		})
	}
	return result
}

// classifyPod returns the (cr, rule, status) triple for a single pod.
func classifyPod(
	pod corev1.Pod,
	ns string,
	inst *v2alpha1.Instrumentation,
	entryByRef map[v2alpha1.WorkloadReference]*v2alpha1.InstrumentedWorkload,
) (cr, rule, status string) {
	ref := resolveWorkloadRef(pod)

	if ref != nil {
		if entry, ok := entryByRef[*ref]; ok && entry.Rollback != nil {
			return inst.Name, entry.RuleName, v2alpha1.StatusRolledBack
		}
	}

	if hasOurLDPreload(pod) {
		ruleName := ""
		if ref != nil {
			if entry, ok := entryByRef[*ref]; ok {
				ruleName = entry.RuleName
			}
		}
		return inst.Name, ruleName, v2alpha1.StatusInstrumented
	}

	matchedRule := findRuleForPod(inst.Spec.Rules, ns, pod.Labels)
	if matchedRule == nil {
		return "", "", v2alpha1.StatusUnmatched
	}

	mode := resolveMode(inst.Spec.Defaults.Mode, matchedRule.Config.Mode)
	if mode == v2alpha1.InstrumentationModeSkip {
		return inst.Name, matchedRule.Name, v2alpha1.StatusSkipped
	}
	return inst.Name, matchedRule.Name, v2alpha1.StatusPendingRestart
}

// findRuleForPod returns the first rule matching the pod's namespace and labels,
// ignoring container-name selectors. Used for pod-level status classification.
func findRuleForPod(rules []v2alpha1.Rule, namespace string, podLabels map[string]string) *v2alpha1.Rule {
	for i := range rules {
		r := &rules[i]
		if matchesNamespace(r.Selector, namespace) && matchesPodLabels(r.Selector, podLabels) {
			return r
		}
	}
	return nil
}
