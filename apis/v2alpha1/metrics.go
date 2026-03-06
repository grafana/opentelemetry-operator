// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package v2alpha1

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	InstrumentedPodsMetricName = "otel.operator.instrumented.pods"
	instrumentedPodsMeterName  = "injector-metrics"

	// OTel semantic convention attribute names (dot-delimited per spec;
	// the Prometheus exporter converts them to underscores automatically).
	AttrNamespace    = "k8s.namespace.name"
	AttrWorkloadKind = "k8s.workload.kind"
	AttrWorkloadName = "k8s.workload.name"
	AttrCR           = "otel.instrumentation.cr"
	AttrRule         = "otel.instrumentation.rule"
	AttrStatus       = "otel.instrumentation.status"

	// Status values reported on the otel.instrumentation.status attribute.
	StatusInstrumented   = "instrumented"
	StatusPendingRestart = "pending_restart"
	StatusRolledBack     = "rolled_back"
	StatusSkipped        = "skipped"
	StatusUnmatched      = "unmatched"
)

// Measurement holds a single data point for the instrumented pods metric.
// +kubebuilder:object:generate=false
type Measurement struct {
	Count int64
	Attrs attribute.Set
}

// InstrumentationMetrics tracks pod instrumentation status via an OTel observable
// up-down counter. Measurements are partitioned by CR name so each reconciler
// instance owns its own slice and can update/delete it independently.
// +kubebuilder:object:generate=false
type InstrumentationMetrics struct {
	mu   sync.RWMutex
	byCR map[string][]Measurement // cr name → current measurements
}

// NewInstrumentationMetrics registers the otel.operator.instrumented.pods
// observable up-down counter with the given MeterProvider and returns an
// InstrumentationMetrics that drives it.
func NewInstrumentationMetrics(mp metric.MeterProvider) (*InstrumentationMetrics, error) {
	im := &InstrumentationMetrics{
		byCR: make(map[string][]Measurement),
	}

	meter := mp.Meter(instrumentedPodsMeterName)
	_, err := meter.Int64ObservableUpDownCounter(
		InstrumentedPodsMetricName,
		metric.WithUnit("{pod}"),
		metric.WithDescription("Number of pods in each instrumentation status"),
		metric.WithInt64Callback(im.observe),
	)
	if err != nil {
		return nil, err
	}

	return im, nil
}

// observe is the OTel SDK callback invoked on each collection cycle.
// When no CRs exist a single zero-valued observation is emitted so the metric
// stays visible in Prometheus (no data points → metric name disappears entirely).
func (im *InstrumentationMetrics) observe(_ context.Context, o metric.Int64Observer) error {
	im.mu.RLock()
	defer im.mu.RUnlock()

	if len(im.byCR) == 0 {
		o.Observe(0, metric.WithAttributeSet(attribute.NewSet()))
		return nil
	}

	for _, measurements := range im.byCR {
		for _, m := range measurements {
			o.Observe(m.Count, metric.WithAttributeSet(m.Attrs))
		}
	}
	return nil
}

// UpdateCR replaces the measurements for the given CR. Called each reconcile cycle.
func (im *InstrumentationMetrics) UpdateCR(crName string, counts []Measurement) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.byCR[crName] = counts
}

// DeleteCR removes the measurements for the given CR. Called when the CR is deleted.
func (im *InstrumentationMetrics) DeleteCR(crName string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	delete(im.byCR, crName)
}
