// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

// newTestMetrics creates an InstrumentationMetrics backed by a ManualReader
// so tests can collect and inspect emitted measurements.
func newTestMetrics(t *testing.T) (*v2alpha1.InstrumentationMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	im, err := v2alpha1.NewInstrumentationMetrics(provider)
	require.NoError(t, err)
	return im, reader
}

// collectDataPoints returns the data points for the instrumented pods metric.
func collectDataPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()
	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == v2alpha1.InstrumentedPodsMetricName {
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "expected Sum[int64]")
				return sum.DataPoints
			}
		}
	}
	return nil
}

// findDataPoint returns the data point whose attribute set matches attrs, or nil.
func findDataPoint(dps []metricdata.DataPoint[int64], attrs attribute.Set) *metricdata.DataPoint[int64] {
	for i := range dps {
		if dps[i].Attributes.Equals(&attrs) {
			return &dps[i]
		}
	}
	return nil
}

// attrsFor builds an attribute set for a Deployment workload (the kind used by all
// test helpers that create pods with ReplicaSet owners).
func attrsFor(ns, name, cr, rule, status string) attribute.Set {
	return attribute.NewSet(
		attribute.String(v2alpha1.AttrNamespace, ns),
		attribute.String(v2alpha1.AttrWorkloadKind, "Deployment"),
		attribute.String(v2alpha1.AttrWorkloadName, name),
		attribute.String(v2alpha1.AttrCR, cr),
		attribute.String(v2alpha1.AttrRule, rule),
		attribute.String(v2alpha1.AttrStatus, status),
	)
}

// --- computeStatusCounts tests ---

func instWithRules(rules ...v2alpha1.Rule) *v2alpha1.Instrumentation {
	return &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-inst"},
		Spec: v2alpha1.InstrumentationSpec{
			Rules: rules,
		},
	}
}

func deploymentPod(ns, rsName, podName string, hasPreload bool, labels map[string]string) corev1.Pod {
	envs := []corev1.EnvVar{}
	if hasPreload {
		envs = []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: rsName,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Env: envs}},
		},
	}
}

func standalonePod(ns, podName string, hasPreload bool) corev1.Pod {
	envs := []corev1.EnvVar{}
	if hasPreload {
		envs = []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Env: envs}},
		},
	}
}

func TestComputeStatusCounts_Instrumented(t *testing.T) {
	pod := deploymentPod("default", "myapp-abc123", "myapp-abc123-xyz", true, nil)
	inst := instWithRules(v2alpha1.Rule{Name: "catch-all"})
	entries := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.Now(),
	}}
	cache := podCache{"default": {pod}}

	measurements := computeStatusCounts(inst, cache, entries)

	require.Len(t, measurements, 1)
	a := measurements[0].Attrs
	assert.Equal(t, int64(1), measurements[0].Count)
	assert.Equal(t, "default", attrValue(a, v2alpha1.AttrNamespace))
	assert.Equal(t, "Deployment", attrValue(a, v2alpha1.AttrWorkloadKind))
	assert.Equal(t, "myapp", attrValue(a, v2alpha1.AttrWorkloadName))
	assert.Equal(t, "test-inst", attrValue(a, v2alpha1.AttrCR))
	assert.Equal(t, "catch-all", attrValue(a, v2alpha1.AttrRule))
	assert.Equal(t, v2alpha1.StatusInstrumented, attrValue(a, v2alpha1.AttrStatus))
}

func TestComputeStatusCounts_PendingRestart(t *testing.T) {
	pod := deploymentPod("default", "myapp-abc123", "myapp-abc123-xyz", false, nil)
	inst := instWithRules(v2alpha1.Rule{Name: "catch-all"})

	measurements := computeStatusCounts(inst, podCache{"default": {pod}}, nil)

	require.Len(t, measurements, 1)
	assert.Equal(t, v2alpha1.StatusPendingRestart, attrValue(measurements[0].Attrs, v2alpha1.AttrStatus))
	assert.Equal(t, "test-inst", attrValue(measurements[0].Attrs, v2alpha1.AttrCR))
	assert.Equal(t, "catch-all", attrValue(measurements[0].Attrs, v2alpha1.AttrRule))
}

func TestComputeStatusCounts_Skipped(t *testing.T) {
	skip := v2alpha1.InstrumentationModeSkip
	pod := deploymentPod("default", "myapp-abc123", "myapp-abc123-xyz", false, nil)
	inst := instWithRules(v2alpha1.Rule{
		Name:   "skip-rule",
		Config: v2alpha1.RuleConfig{Mode: &skip},
	})

	measurements := computeStatusCounts(inst, podCache{"default": {pod}}, nil)

	require.Len(t, measurements, 1)
	assert.Equal(t, v2alpha1.StatusSkipped, attrValue(measurements[0].Attrs, v2alpha1.AttrStatus))
	assert.Equal(t, "skip-rule", attrValue(measurements[0].Attrs, v2alpha1.AttrRule))
}

func TestComputeStatusCounts_Unmatched(t *testing.T) {
	// Rule only matches "production", so the pod in "staging" is unmatched.
	pod := deploymentPod("staging", "otherapp-def456", "otherapp-def456-xyz", false, nil)
	inst := instWithRules(v2alpha1.Rule{
		Name:     "targeted",
		Selector: v2alpha1.RuleSelector{Namespaces: []string{"production"}},
	})

	measurements := computeStatusCounts(inst, podCache{"staging": {pod}}, nil)

	require.Len(t, measurements, 1)
	assert.Equal(t, v2alpha1.StatusUnmatched, attrValue(measurements[0].Attrs, v2alpha1.AttrStatus))
	assert.Equal(t, "", attrValue(measurements[0].Attrs, v2alpha1.AttrCR))
	assert.Equal(t, "", attrValue(measurements[0].Attrs, v2alpha1.AttrRule))
}

func TestComputeStatusCounts_RolledBack(t *testing.T) {
	pod := deploymentPod("default", "myapp-abc123", "myapp-abc123-xyz", true, nil)
	inst := instWithRules(v2alpha1.Rule{Name: "catch-all"})
	entries := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef: v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:    "catch-all",
		Rollback:    &v2alpha1.RollbackInfo{Reason: "CrashLoopBackOff"},
	}}

	measurements := computeStatusCounts(inst, podCache{"default": {pod}}, entries)

	require.Len(t, measurements, 1)
	assert.Equal(t, v2alpha1.StatusRolledBack, attrValue(measurements[0].Attrs, v2alpha1.AttrStatus))
	assert.Equal(t, "test-inst", attrValue(measurements[0].Attrs, v2alpha1.AttrCR))
	assert.Equal(t, "catch-all", attrValue(measurements[0].Attrs, v2alpha1.AttrRule))
}

func TestComputeStatusCounts_StandalonePod(t *testing.T) {
	pod := standalonePod("default", "standalone", true)
	inst := instWithRules(v2alpha1.Rule{Name: "catch-all"})

	measurements := computeStatusCounts(inst, podCache{"default": {pod}}, nil)

	require.Len(t, measurements, 1)
	assert.Equal(t, "Pod", attrValue(measurements[0].Attrs, v2alpha1.AttrWorkloadKind))
	assert.Equal(t, "standalone", attrValue(measurements[0].Attrs, v2alpha1.AttrWorkloadName))
	assert.Equal(t, v2alpha1.StatusInstrumented, attrValue(measurements[0].Attrs, v2alpha1.AttrStatus))
}

func TestComputeStatusCounts_LabelSelector(t *testing.T) {
	// Rule matches only pods with app=frontend; other pods are unmatched.
	matchingPod := deploymentPod("default", "frontend-abc123", "frontend-abc123-x1", false, map[string]string{"app": "frontend"})
	otherPod := deploymentPod("default", "myapp-abc123", "myapp-abc123-x1", false, nil)
	inst := instWithRules(v2alpha1.Rule{
		Name:     "frontend-rule",
		Selector: v2alpha1.RuleSelector{PodLabels: map[string]string{"app": "frontend"}},
	})

	measurements := computeStatusCounts(inst, podCache{"default": {matchingPod, otherPod}}, nil)

	require.Len(t, measurements, 2)
	statuses := map[string]string{}
	for _, m := range measurements {
		statuses[attrValue(m.Attrs, v2alpha1.AttrWorkloadName)] = attrValue(m.Attrs, v2alpha1.AttrStatus)
	}
	assert.Equal(t, v2alpha1.StatusPendingRestart, statuses["frontend"])
	assert.Equal(t, v2alpha1.StatusUnmatched, statuses["myapp"])
}

func TestComputeStatusCounts_MultiplePodsAggregated(t *testing.T) {
	// Two pods from the same Deployment should aggregate into one data point.
	p1 := deploymentPod("default", "myapp-abc123", "myapp-abc123-x1", true, nil)
	p2 := deploymentPod("default", "myapp-abc123", "myapp-abc123-x2", true, nil)
	inst := instWithRules(v2alpha1.Rule{Name: "catch-all"})
	entries := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef: v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:    "catch-all",
	}}

	measurements := computeStatusCounts(inst, podCache{"default": {p1, p2}}, entries)

	require.Len(t, measurements, 1)
	assert.Equal(t, int64(2), measurements[0].Count)
}

// attrValue extracts a string attribute value from an attribute.Set.
func attrValue(attrs attribute.Set, key string) string {
	v, ok := attrs.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.AsString()
}

// --- UpdateCR / DeleteCR / observe tests ---

func TestInstrumentationMetrics_UpdateAndObserve(t *testing.T) {
	im, reader := newTestMetrics(t)

	a := attrsFor("default", "myapp", "inst1", "catch-all", v2alpha1.StatusInstrumented)
	im.UpdateCR("inst1", []v2alpha1.Measurement{{Count: 3, Attrs: a}})

	dps := collectDataPoints(t, reader)
	dp := findDataPoint(dps, a)
	require.NotNil(t, dp)
	assert.Equal(t, int64(3), dp.Value)
}

func TestInstrumentationMetrics_UpdateCR_Replaces(t *testing.T) {
	im, reader := newTestMetrics(t)

	a1 := attrsFor("default", "myapp", "inst1", "r1", v2alpha1.StatusInstrumented)
	a2 := attrsFor("default", "otherapp", "inst1", "r1", v2alpha1.StatusPendingRestart)

	im.UpdateCR("inst1", []v2alpha1.Measurement{{Count: 2, Attrs: a1}})
	// Replace with different data.
	im.UpdateCR("inst1", []v2alpha1.Measurement{{Count: 1, Attrs: a2}})

	dps := collectDataPoints(t, reader)
	// Old series gone, new one present.
	assert.Nil(t, findDataPoint(dps, a1))
	dp := findDataPoint(dps, a2)
	require.NotNil(t, dp)
	assert.Equal(t, int64(1), dp.Value)
}

func TestInstrumentationMetrics_DeleteCR(t *testing.T) {
	im, reader := newTestMetrics(t)

	a := attrsFor("default", "myapp", "inst1", "catch-all", v2alpha1.StatusInstrumented)
	im.UpdateCR("inst1", []v2alpha1.Measurement{{Count: 5, Attrs: a}})
	im.DeleteCR("inst1")

	dps := collectDataPoints(t, reader)
	assert.Nil(t, findDataPoint(dps, a))
	// When byCR is empty a sentinel zero is emitted so the metric stays visible.
	require.Len(t, dps, 1)
	assert.Equal(t, int64(0), dps[0].Value)
}

func TestInstrumentationMetrics_MultipleCRs(t *testing.T) {
	im, reader := newTestMetrics(t)

	a1 := attrsFor("ns1", "app1", "cr1", "rule1", v2alpha1.StatusInstrumented)
	a2 := attrsFor("ns2", "app2", "cr2", "rule2", v2alpha1.StatusPendingRestart)

	im.UpdateCR("cr1", []v2alpha1.Measurement{{Count: 2, Attrs: a1}})
	im.UpdateCR("cr2", []v2alpha1.Measurement{{Count: 4, Attrs: a2}})

	dps := collectDataPoints(t, reader)
	dp1 := findDataPoint(dps, a1)
	dp2 := findDataPoint(dps, a2)
	require.NotNil(t, dp1)
	require.NotNil(t, dp2)
	assert.Equal(t, int64(2), dp1.Value)
	assert.Equal(t, int64(4), dp2.Value)

	// Deleting cr1 should not affect cr2.
	im.DeleteCR("cr1")
	dps = collectDataPoints(t, reader)
	assert.Nil(t, findDataPoint(dps, a1))
	dp2 = findDataPoint(dps, a2)
	require.NotNil(t, dp2)
	assert.Equal(t, int64(4), dp2.Value)
}

// --- nil-safety test ---

func TestRollbackReconciler_NilMetrics_DoesNotPanic(t *testing.T) {
	inst := testInstrumentation(1)
	ns := testNamespace()
	fc := clocktesting.NewFakeClock(metav1.Now().Time)
	r, _ := newRollbackReconciler(fc, inst, ns)

	// nil metrics — should not panic during reconcile.
	r.metrics = nil
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: inst.Name},
	})
	assert.NoError(t, err)
}
