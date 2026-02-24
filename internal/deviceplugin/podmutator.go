// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package deviceplugin provides a PodMutator that injects device resource
// requests for Kubernetes device-plugin-based auto-instrumentation.
// Instead of init containers + ephemeral volumes, this approach requests
// a virtual device resource that triggers kubelet to call the device
// plugin's Allocate(), which mounts the agent files into the container.
package deviceplugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"
	"github.com/open-telemetry/opentelemetry-operator/pkg/constants"
)

const (
	annotationInjectJava = "instrumentation.opentelemetry.io/inject-java"

	// The device resource name must match what the device plugin DaemonSet registers.
	deviceResourceName corev1.ResourceName = "instrumentation.opentelemetry.io/java"

	// Mount path where the device plugin mounts the agent JAR.
	agentMountPath = "/otel-auto-instrumentation-java"

	envJavaToolOptions = "JAVA_TOOL_OPTIONS"
)

// podMutator injects device resource requests for Java auto-instrumentation.
type podMutator struct {
	Logger logr.Logger
	Client client.Client
}

// NewMutator creates a new PodMutator for device-plugin-based instrumentation.
func NewMutator(logger logr.Logger, client client.Client) *podMutator {
	return &podMutator{
		Logger: logger.WithName("device-plugin-mutator"),
		Client: client,
	}
}

func (m *podMutator) Mutate(ctx context.Context, ns corev1.Namespace, pod corev1.Pod) (corev1.Pod, error) {
	logger := m.Logger.WithValues("namespace", ns.Name)
	if pod.Name != "" {
		logger = logger.WithValues("name", pod.Name)
	} else if pod.GenerateName != "" {
		logger = logger.WithValues("generateName", pod.GenerateName)
	}

	// Check if pod/namespace is opted in for Java device-plugin instrumentation.
	instValue := annotationValue(ns, pod, annotationInjectJava)
	if instValue == "" || strings.EqualFold(instValue, "false") {
		return pod, nil
	}

	// Look up the Instrumentation CR to get exporter config and env vars.
	inst, err := m.getInstrumentation(ctx, ns, instValue)
	if err != nil {
		logger.Error(err, "failed to get Instrumentation CR for device-plugin injection")
		return pod, nil // Don't block pod creation.
	}
	if inst == nil {
		return pod, nil
	}

	logger.Info("injecting device-plugin instrumentation", "instrumentation", inst.Name)

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]

		// Inject device resource request.
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		container.Resources.Limits[deviceResourceName] = resource.MustParse("1")
		container.Resources.Requests[deviceResourceName] = resource.MustParse("1")

		// Inject JAVA_TOOL_OPTIONS.
		javaagentArg := fmt.Sprintf("-javaagent:%s/opentelemetry-javaagent.jar", agentMountPath)
		appendEnvVar(container, envJavaToolOptions, javaagentArg)

		// Inject standard OTEL env vars from the Instrumentation CR.
		injectOTELEnvVars(container, inst, pod)
	}

	return pod, nil
}

// annotationValue returns the effective annotation value, checking pod then namespace.
func annotationValue(ns corev1.Namespace, pod corev1.Pod, annotation string) string {
	if v := pod.Annotations[annotation]; v != "" {
		return v
	}
	return ns.Annotations[annotation]
}

// getInstrumentation looks up the Instrumentation CR referenced by the annotation.
func (m *podMutator) getInstrumentation(ctx context.Context, ns corev1.Namespace, instValue string) (*v1alpha1.Instrumentation, error) {
	var nsName types.NamespacedName

	if strings.EqualFold(instValue, "true") {
		// Find the single Instrumentation in this namespace.
		var list v1alpha1.InstrumentationList
		if err := m.Client.List(ctx, &list, client.InNamespace(ns.Name)); err != nil {
			return nil, err
		}
		switch len(list.Items) {
		case 0:
			return nil, nil
		case 1:
			return &list.Items[0], nil
		default:
			return nil, fmt.Errorf("multiple Instrumentation instances in namespace %s", ns.Name)
		}
	}

	// Explicit name reference.
	if instNs, instName, ok := strings.Cut(instValue, "/"); ok {
		nsName = types.NamespacedName{Name: instName, Namespace: instNs}
	} else {
		nsName = types.NamespacedName{Name: instValue, Namespace: ns.Name}
	}

	inst := &v1alpha1.Instrumentation{}
	if err := m.Client.Get(ctx, nsName, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// injectOTELEnvVars adds standard OpenTelemetry environment variables.
func injectOTELEnvVars(container *corev1.Container, inst *v1alpha1.Instrumentation, pod corev1.Pod) {
	// Service name: use annotation, label, or pod name.
	serviceName := serviceNameFromPod(pod)
	setEnvVarIfNotPresent(container, constants.EnvOTELServiceName, serviceName)

	// Exporter endpoint.
	if inst.Spec.Endpoint != "" {
		setEnvVarIfNotPresent(container, constants.EnvOTELExporterOTLPEndpoint, inst.Spec.Endpoint)
	}

	// Propagators.
	if len(inst.Spec.Propagators) > 0 {
		propagators := make([]string, len(inst.Spec.Propagators))
		for i, p := range inst.Spec.Propagators {
			propagators[i] = string(p)
		}
		setEnvVarIfNotPresent(container, constants.EnvOTELPropagators, strings.Join(propagators, ","))
	}

	// Sampler.
	if inst.Spec.Type != "" {
		setEnvVarIfNotPresent(container, constants.EnvOTELTracesSampler, string(inst.Spec.Type))
	}
	if inst.Spec.Argument != "" {
		setEnvVarIfNotPresent(container, constants.EnvOTELTracesSamplerArg, inst.Spec.Argument)
	}

	// Resource attributes: pod name, node name via downward API.
	setEnvVarIfNotPresent(container, constants.EnvPodName, "")
	setPodFieldEnvVar(container, constants.EnvPodName, "metadata.name")
	setPodFieldEnvVar(container, constants.EnvNodeName, "spec.nodeName")

	// Custom env vars from the Instrumentation CR.
	for _, env := range inst.Spec.Env {
		setEnvVarIfNotPresent(container, env.Name, env.Value)
	}

	// Java-specific env vars from the Instrumentation CR.
	for _, env := range inst.Spec.Java.Env {
		setEnvVarIfNotPresent(container, env.Name, env.Value)
	}
}

// serviceNameFromPod derives the service name from pod metadata.
func serviceNameFromPod(pod corev1.Pod) string {
	// Try well-known labels first.
	for _, label := range constants.LabelAppName {
		if v, ok := pod.Labels[label]; ok && v != "" {
			return v
		}
	}
	// Fall back to pod name or generateName.
	if pod.Name != "" {
		return pod.Name
	}
	return strings.TrimSuffix(pod.GenerateName, "-")
}

// appendEnvVar appends a value to an existing env var or creates a new one.
func appendEnvVar(container *corev1.Container, name, value string) {
	for i, env := range container.Env {
		if env.Name == name {
			if env.ValueFrom != nil {
				return // Can't append to ValueFrom.
			}
			container.Env[i].Value = env.Value + " " + value
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}

// setEnvVarIfNotPresent adds an env var only if it's not already set.
func setEnvVarIfNotPresent(container *corev1.Container, name, value string) {
	for _, env := range container.Env {
		if env.Name == name {
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}

// setPodFieldEnvVar sets an env var from the downward API.
func setPodFieldEnvVar(container *corev1.Container, name, fieldPath string) {
	for _, env := range container.Env {
		if env.Name == name {
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: fieldPath,
			},
		},
	})
}
