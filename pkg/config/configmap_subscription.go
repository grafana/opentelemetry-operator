// Copyright (c) Saswata Mukherjee (@saswatamcode)
// Licensed under the Apache License 2.0.

package config

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"slices"
)

type configMapSubscriptionMetrics struct {
	configMapGauge                 *prometheus.GaugeVec
	configMapHTTPRequestsPerformed *prometheus.CounterVec
	configMapHTTPRequestsLatency   *prometheus.HistogramVec
	configMapFileReadsPerformed    *prometheus.CounterVec
	configMapFileReadsLatency      *prometheus.HistogramVec
}

func newConfigMapSubscriptionMetrics() *configMapSubscriptionMetrics {
	c := &configMapSubscriptionMetrics{}

	c.configMapHTTPRequestsPerformed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "configmap_operator_http_requests_total",
		Help: "The total number of HTTP GET requests for fetching ConfigMap source data.",
	}, []string{"domain"})

	c.configMapHTTPRequestsLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "configmap_operator_per_http_request_latency",
		Help:    "Latency for HTTP GET requests.",
		Buckets: prometheus.DefBuckets,
	}, []string{"domain"})

	c.configMapFileReadsPerformed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "configmap_operator_file_read_total",
		Help: "The total number of file reads for fetching ConfigMap source data.",
	}, []string{"filepath"})

	c.configMapFileReadsLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "configmap_operator_per_file_read_latency",
		Help:    "Latency for file reads.",
		Buckets: prometheus.DefBuckets,
	}, []string{"filepath"})

	c.configMapGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "configmap_operator_current_configmaps",
		Help: "The total number of ConfigMaps that are being updated at a time.",
	}, []string{"name", "namespace"})

	return c
}

type ConfigMapLoader struct {
	Config    Config
	Ctx       context.Context
	Logger    logr.Logger
	ClientSet kubernetes.Interface
	Namespace string

	watcherInterface watch.Interface
	metrics          *configMapSubscriptionMetrics
}

func (c *ConfigMapLoader) Reconcile(object runtime.Object, event watch.EventType) {
	configMap := object.(*v1.ConfigMap)

	rootSpan := opentracing.GlobalTracer().StartSpan("configMapSubscriptionReconcile")
	rootSpan.SetTag("configMap name", configMap.Name)
	rootSpan.SetTag("configMap namespace", configMap.Namespace)
	defer rootSpan.Finish()

	c.Logger.Info("ConfigMap subscription event", event, "ConfigMap name", configMap.Name)

	switch event {
	case watch.Modified:
		watchEventAddSpan := opentracing.GlobalTracer().StartSpan(
			"watchEventAdd", opentracing.ChildOf(rootSpan.Context()),
		)
		watchEventAddSpan.SetTag("configMap name", configMap.Name)
		watchEventAddSpan.SetTag("configMap namespace", configMap.Namespace)
		defer watchEventAddSpan.Finish()

		err := c.loadConfig(configMap)
		if err != nil {
			c.Logger.Error(err, "error loading config")
			return
		}
	}
}

func (c *ConfigMapLoader) Subscribe() (watch.Interface, error) {

	var err error
	c.watcherInterface, err = c.ClientSet.CoreV1().ConfigMaps(c.Namespace).Watch(c.Ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", configMapName),
	})
	if err != nil {
		return nil, err
	}

	c.metrics = newConfigMapSubscriptionMetrics()

	return c.watcherInterface, nil
}

func (c *ConfigMapLoader) IsPodEnabled(pod v1.Pod) bool {
	attrs := getPodAttrs(pod)
	for _, service := range c.Config.Discovery.Services {
		if matchByAttributes(&attrs, &service) {
			return true
		}
	}
	return false
}

func (c *ConfigMapLoader) loadConfig(configMap *v1.ConfigMap) error {
	s := configMap.Data[configMapKey]
	newConfig := Config{}
	err := yaml.Unmarshal([]byte(s), &newConfig)
	if err != nil {
		return fmt.Errorf("error unmarshaling YAML: %w", err)
	}
	c.Logger.Info("loaded config")
	err = newConfig.Validate()
	if err != nil {
		return fmt.Errorf("error validating config: %w", err)
	}

	oldConfig := c.Config
	remove, add := diff(oldConfig, newConfig)

	c.restartAll(remove)
	c.restartAll(add)

	// get all pods that match the new added/removed services
	// restart those pods
	// when they are restarted, they will pick up the new config from the admission controller

	// how to see if the pod is currently instrumented:
	// https://github.com/grafana/opentelemetry-operator/blob/4da4f66e0a4d59f0f99a6b6b3fcbd68523e0506c/pkg/instrumentation/helper.go#L46

	//matchByAttributes(&attrs, &newConfig.Discovery.Services[0])

	c.Config = newConfig

	return err
}

func diff(oldConfig Config, newConfig Config) (DefinitionCriteria, DefinitionCriteria) {
	var remove DefinitionCriteria
	for _, old := range oldConfig.Discovery.Services {
		if !slices.ContainsFunc(newConfig.Discovery.Services, func(attributes Attributes) bool {
			return equals(attributes, old)
		}) {
			remove = append(remove, old)
		}
	}
	var add DefinitionCriteria
	for _, n := range newConfig.Discovery.Services {
		if !slices.ContainsFunc(oldConfig.Discovery.Services, func(attributes Attributes) bool {
			return equals(attributes, n)
		}) {
			add = append(add, n)
		}
	}

	// added config parts: add instrumentation
	// removed config parts: remove instrumentation
	return remove, add
}

func equals(a, b Attributes) bool {
	return a.Name == b.Name && a.Namespace == b.Namespace && regexMapEquals(a.Metadata, b.Metadata) && regexMapEquals(a.PodLabels, b.PodLabels)
}
func regexMapEquals(a, b map[string]*RegexpAttr) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		v2, ok := b[k]
		if !ok {
			return false
		}
		if v.re.String() != v2.re.String() {
			return false
		}
	}
	return true
}

func (c *ConfigMapLoader) restartAll(remove DefinitionCriteria) {
	//for _, attributes := range remove {
	// do we need to get all pods or deployment to call matchByAttributes?
	//matchByAttributes()
	//}
	// use https://stackoverflow.com/questions/61335318/how-to-restart-a-deployment-in-kubernetes-using-go-client
}

func (c *ConfigMapLoader) restartPod() {
	// see https://stackoverflow.com/questions/61335318/how-to-restart-a-deployment-in-kubernetes-using-go-client
}
