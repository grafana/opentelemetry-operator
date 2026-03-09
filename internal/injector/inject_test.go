// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

func TestInjectPod_Basic(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "ghcr.io/example/otel-injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "catch-all",
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{
							{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel-collector:4318"},
							{Name: "OTEL_TRACES_SAMPLER", Value: "parentbased_traceidratio"},
							{Name: "OTEL_TRACES_SAMPLER_ARG", Value: "0.25"},
							{Name: "OTEL_PROPAGATORS", Value: "tracecontext,baggage"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// No init containers — image volumes replace the copy approach.
	assert.Empty(t, result.Spec.InitContainers)

	// Image volume added for the injector.
	require.Len(t, result.Spec.Volumes, 1)
	assert.Equal(t, volumeName, result.Spec.Volumes[0].Name)
	require.NotNil(t, result.Spec.Volumes[0].Image)
	assert.Equal(t, "ghcr.io/example/otel-injector:latest", result.Spec.Volumes[0].Image.Reference)
	assert.Equal(t, corev1.PullIfNotPresent, result.Spec.Volumes[0].Image.PullPolicy)

	// Container has volume mount (read-only)
	require.Len(t, result.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, mountPath, result.Spec.Containers[0].VolumeMounts[0].MountPath)
	assert.True(t, result.Spec.Containers[0].VolumeMounts[0].ReadOnly)

	// Check env vars
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Equal(t, configFilePath, envMap[envInjectorConfigFile])
	assert.Equal(t, "http/protobuf", envMap[envOTLPProtocol])
	assert.Equal(t, "http://otel-collector:4318", envMap["OTEL_EXPORTER_OTLP_ENDPOINT"])
	assert.Equal(t, "parentbased_traceidratio", envMap["OTEL_TRACES_SAMPLER"])
	assert.Equal(t, "0.25", envMap["OTEL_TRACES_SAMPLER_ARG"])
	assert.Equal(t, "tracecontext,baggage", envMap["OTEL_PROPAGATORS"])
	assert.Equal(t, "app", envMap[envInjectorK8sContainerName])
	assert.Equal(t, "test-pod", envMap[envInjectorServiceName])
	assert.Equal(t, "default", envMap[envInjectorServiceNamespace])

	// Downward API refs
	for _, name := range []string{envInjectorK8sNamespace, envInjectorK8sPodName, envInjectorK8sPodUID} {
		found := findEnv(result.Spec.Containers[0].Env, name)
		require.NotNil(t, found, "expected env var %s", name)
		assert.NotNil(t, found.ValueFrom, "expected ValueFrom for %s", name)
	}
}

func TestInjectPod_RuleMatchesByNamespace(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "prod-only",
					Selector: v2alpha1.RuleSelector{
						Namespaces: []string{"production"},
					},
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{{Name: "ENV", Value: "prod"}},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	// Should not match — pod is in "default", rule targets "production"
	result := mustInjectPod(t, inst, pod, "default")
	assert.Empty(t, result.Spec.Containers[0].Env)
	assert.Empty(t, result.Spec.Volumes)

	// Should match
	result = mustInjectPod(t, inst, pod, "production")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "prod", envMap["ENV"])
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
}

func TestInjectPod_RuleMatchesByPodLabels(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Selector: v2alpha1.RuleSelector{
						PodLabels: map[string]string{"app": "frontend"},
					},
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{{Name: "ROLE", Value: "frontend"}},
					},
				},
			},
		},
	}

	// No labels — should not match
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	result := mustInjectPod(t, inst, pod, "default")
	assert.Empty(t, result.Spec.Containers[0].Env)

	// Matching labels
	pod.Labels = map[string]string{"app": "frontend", "version": "v1"}
	result = mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "frontend", envMap["ROLE"])
}

func TestInjectPod_RuleMatchesByContainerName(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Selector: v2alpha1.RuleSelector{
						ContainerNames: []string{"sidecar"},
					},
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{{Name: "TARGET", Value: "sidecar"}},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// "app" should NOT be injected (no matching rule)
	assert.Empty(t, result.Spec.Containers[0].Env)
	assert.Empty(t, result.Spec.Containers[0].VolumeMounts)

	// "sidecar" should be injected
	envMap := envToMap(result.Spec.Containers[1].Env)
	assert.Equal(t, "sidecar", envMap["TARGET"])
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Len(t, result.Spec.Containers[1].VolumeMounts, 1)
}

func TestInjectPod_ModeSkip(t *testing.T) {
	skip := v2alpha1.InstrumentationModeSkip
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "opt-out-sidecar",
					Selector: v2alpha1.RuleSelector{
						ContainerNames: []string{"sidecar"},
					},
					Config: v2alpha1.RuleConfig{Mode: &skip},
				},
				{
					Name: "catch-all",
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// "app" matches catch-all → injected
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])

	// "sidecar" matches Skip rule → NOT injected
	assert.Empty(t, result.Spec.Containers[1].Env)
	assert.Empty(t, result.Spec.Containers[1].VolumeMounts)
}

func TestInjectPod_ModeInstall(t *testing.T) {
	install := v2alpha1.InstrumentationModeInstall
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name:   "force-install",
					Config: v2alpha1.RuleConfig{Mode: &install},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Equal(t, "install", envMap[envInjectorMode])
}

func TestInjectPod_DefaultModeIsInstallUnlessConflict(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "install_unless_conflict", envMap[envInjectorMode])
}

func TestInjectPod_CRDefaultModeOverriddenByRule(t *testing.T) {
	skip := v2alpha1.InstrumentationModeSkip
	install := v2alpha1.InstrumentationModeInstall
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Defaults: v2alpha1.InstrumentationDefaults{Mode: &skip},
			Rules: []v2alpha1.Rule{
				{
					Name:   "force-install",
					Config: v2alpha1.RuleConfig{Mode: &install},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	// Rule-level Install overrides CR-level Skip
	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Equal(t, "install", envMap[envInjectorMode])
}

func TestInjectPod_CRDefaultModeAppliesWhenRuleUnset(t *testing.T) {
	install := v2alpha1.InstrumentationModeInstall
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Defaults: v2alpha1.InstrumentationDefaults{Mode: &install},
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "install", envMap[envInjectorMode])
}

func TestInjectPod_FirstMatchWins(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "specific",
					Selector: v2alpha1.RuleSelector{
						PodLabels: map[string]string{"app": "api"},
					},
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{{Name: "MATCHED", Value: "specific"}},
					},
				},
				{
					Name: "catch-all",
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{{Name: "MATCHED", Value: "catch-all"}},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test",
			Labels: map[string]string{"app": "api"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "specific", envMap["MATCHED"])
}

func TestInjectPod_SkipsContainerWithExistingLDPreload(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "already-instrumented",
					Image: "app:latest",
					Env:   []corev1.EnvVar{{Name: envLDPreload, Value: "/other/lib.so"}},
				},
				{
					Name:  "not-instrumented",
					Image: "app2:latest",
				},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// First container should not get extra env vars
	assert.Len(t, result.Spec.Containers[0].Env, 1)
	assert.Empty(t, result.Spec.Containers[0].VolumeMounts)

	// Second container should be injected
	envMap := envToMap(result.Spec.Containers[1].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Len(t, result.Spec.Containers[1].VolumeMounts, 1)
}

func TestInjectPod_CatchAllSkipsSystemNamespaces(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{Name: "catch-all"}, // empty selector = all non-system namespaces
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "coredns"}},
		},
	}

	// System namespaces should not match
	result := mustInjectPod(t, inst, pod, "kube-system")
	assert.Empty(t, result.Spec.Containers[0].Env)
	assert.Empty(t, result.Spec.Volumes)

	// Non-system namespaces should match (CR is cluster-scoped)
	result = mustInjectPod(t, inst, pod, "other-namespace")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])

	result = mustInjectPod(t, inst, pod, "default")
	envMap = envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
}

func TestMatchesNamespace_ExplicitSystemNamespace(t *testing.T) {
	// If a rule explicitly lists kube-system, it should still match
	// (the filter only applies to catch-all rules).
	sel := v2alpha1.RuleSelector{Namespaces: []string{"kube-system"}}
	assert.True(t, matchesNamespace(sel, "kube-system"))
}

func TestInjectPod_NoMatchingRules(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Selector: v2alpha1.RuleSelector{
						Namespaces: []string{"production"},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	// No matching rule → container should not be touched
	assert.Empty(t, result.Spec.Containers[0].Env)
	assert.Empty(t, result.Spec.Containers[0].VolumeMounts)
	// No init container or volume should be added when nothing matches.
	assert.Empty(t, result.Spec.InitContainers)
	assert.Empty(t, result.Spec.Volumes)
}

func TestIsAlreadyInjected_ImageVolume(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						Image: &corev1.ImageVolumeSource{
							Reference: "injector:latest",
						},
					},
				},
			},
		},
	}
	assert.True(t, isAlreadyInjected(pod))
}

func TestIsAlreadyInjected_LDPreload(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Env:  []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}},
				},
			},
		},
	}
	assert.True(t, isAlreadyInjected(pod))
}

func TestIsAlreadyInjected_Clean(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	assert.False(t, isAlreadyInjected(pod))
}

func TestDeriveServiceName_Deployment(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "myapp-7b9f5d4c8f"},
			},
		},
	}
	assert.Equal(t, "myapp", deriveServiceName(pod))
}

func TestDeriveServiceName_ReplicaSetWithDeploymentHash(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-deployment-abc123"},
			},
		},
	}
	assert.Equal(t, "my-deployment", deriveServiceName(pod))
}

func TestDeriveServiceName_StatefulSet(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-statefulset"},
			},
		},
	}
	assert.Equal(t, "my-statefulset", deriveServiceName(pod))
}

func TestDeriveServiceName_FallbackToPodName(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "standalone-pod",
		},
	}
	assert.Equal(t, "standalone-pod", deriveServiceName(pod))
}

func TestDeriveServiceName_NoPodName(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "job-runner-",
		},
	}
	// No owner refs, no pod name → empty; buildEnvVars falls back to container name.
	assert.Equal(t, "", deriveServiceName(pod))
}

func TestServiceNameFallsBackToContainerName(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	// Pod with no owner refs and no name — service name should fall back to container name.
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "runner-"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	svcName := findEnv(result.Spec.Containers[0].Env, envInjectorServiceName)
	require.NotNil(t, svcName)
	assert.Equal(t, "worker", svcName.Value)
}

func TestInjectPod_EmptyRuleConfig(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)

	// Core env vars should always be set
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
	assert.Equal(t, configFilePath, envMap[envInjectorConfigFile])
	assert.Equal(t, "http/protobuf", envMap[envOTLPProtocol])
}

func TestInjectPod_OTLPProtocolDefault(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	// Default: OTLP protocol is injected as http/protobuf
	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, "http/protobuf", envMap[envOTLPProtocol])
}

func TestInjectPod_OTLPProtocolUserOverride(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "grpc-rule",
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{
							{Name: envOTLPProtocol, Value: "grpc"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// User's value must win and there must be no duplicate.
	envs := result.Spec.Containers[0].Env
	assert.Equal(t, 1, countEnv(envs, envOTLPProtocol), "expected exactly one %s", envOTLPProtocol)
	assert.Equal(t, "grpc", findEnv(envs, envOTLPProtocol).Value)
}

func TestInjectPod_RejectsOtelInjectorEnvVars(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "sneaky-rule",
					Config: v2alpha1.RuleConfig{
						Env: []corev1.EnvVar{
							{Name: "OTEL_INJECTOR_SERVICE_NAME", Value: "hacked"},
							{Name: "OTEL_TRACES_SAMPLER", Value: "always_on"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	_, err := injectPod(inst, pod, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_INJECTOR_SERVICE_NAME")
	assert.Contains(t, err.Error(), "sneaky-rule")
}

func TestInjectPod_ResourceAttributes(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123-xyz",
			Namespace: "production",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "myapp-abc123"},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "production")
	envs := result.Spec.Containers[0].Env

	resAttrs := findEnv(envs, envInjectorResourceAttributes)
	require.NotNil(t, resAttrs)

	// service.instance.id uses $(...) references for runtime expansion
	assert.Contains(t, resAttrs.Value, "service.instance.id=$(OTEL_INJECTOR_K8S_NAMESPACE_NAME).$(OTEL_INJECTOR_K8S_POD_NAME).server")
	// k8s.node.name uses $(...) reference
	assert.Contains(t, resAttrs.Value, "k8s.node.name=$(OTEL_NODE_NAME)")
	// Owner ref produces k8s.replicaset.name and derived k8s.deployment.name
	assert.Contains(t, resAttrs.Value, "k8s.replicaset.name=myapp-abc123")
	assert.Contains(t, resAttrs.Value, "k8s.deployment.name=myapp")

	// Verify OTEL_NODE_NAME downward API var is set
	nodeName := findEnv(envs, envNodeName)
	require.NotNil(t, nodeName)
	assert.Equal(t, "spec.nodeName", nodeName.ValueFrom.FieldRef.FieldPath)
}

func TestBuildInjectorResourceAttrs_Deployment(t *testing.T) {
	ownerRefs := []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "web-abc123"},
	}
	attrs := buildInjectorResourceAttrs("app", ownerRefs)
	assert.Contains(t, attrs, "k8s.replicaset.name=web-abc123")
	assert.Contains(t, attrs, "k8s.deployment.name=web")
	assert.Contains(t, attrs, "service.instance.id=")
}

func TestBuildInjectorResourceAttrs_CronJob(t *testing.T) {
	ownerRefs := []metav1.OwnerReference{
		{Kind: "Job", Name: "cleanup-28450380"},
	}
	attrs := buildInjectorResourceAttrs("worker", ownerRefs)
	assert.Contains(t, attrs, "k8s.job.name=cleanup-28450380")
	assert.Contains(t, attrs, "k8s.cronjob.name=cleanup")
}

func TestBuildInjectorResourceAttrs_StatefulSet(t *testing.T) {
	ownerRefs := []metav1.OwnerReference{
		{Kind: "StatefulSet", Name: "redis"},
	}
	attrs := buildInjectorResourceAttrs("redis", ownerRefs)
	assert.Contains(t, attrs, "k8s.statefulset.name=redis")
}

func TestBuildInjectorResourceAttrs_NoOwner(t *testing.T) {
	attrs := buildInjectorResourceAttrs("app", nil)
	assert.Contains(t, attrs, "service.instance.id=")
	assert.Contains(t, attrs, "k8s.node.name=")
	assert.NotContains(t, attrs, "k8s.replicaset.name")
}

func TestInjectPod_DeclarativeConfig_MountsConfigMapAndSetsEnv(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "my-inst"},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "with-config",
					Config: v2alpha1.RuleConfig{
						DeclarativeConfig: &v2alpha1.DeclarativeConfig{
							Object: map[string]any{"file_format": "1.0"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// Verify config volume was added.
	var configVol *corev1.Volume
	for i := range result.Spec.Volumes {
		if result.Spec.Volumes[i].ConfigMap != nil {
			configVol = &result.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, configVol, "expected a ConfigMap volume")
	assert.Equal(t, ConfigMapName("my-inst", "with-config"), configVol.ConfigMap.Name)

	// Verify config volume mount.
	c := result.Spec.Containers[0]
	var configMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == configMountPath {
			configMount = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, configMount, "expected config volume mount")
	assert.True(t, configMount.ReadOnly)

	// Verify both config file env vars are set.
	envMap := envToMap(c.Env)
	assert.Equal(t, otelConfigFilePath, envMap[envOTelConfigFile])
	assert.Equal(t, otelConfigFilePath, envMap[envOTelExperimentalConfigFile])
}

func TestInjectPod_RejectsConfigFileEnvVars(t *testing.T) {
	for _, envName := range []string{envOTelConfigFile, envOTelExperimentalConfigFile} {
		t.Run(envName, func(t *testing.T) {
			inst := &v2alpha1.Instrumentation{
				Spec: v2alpha1.InstrumentationSpec{
					Injector: "injector:latest",
					Rules: []v2alpha1.Rule{
						{
							Name: "bad-rule",
							Config: v2alpha1.RuleConfig{
								Env: []corev1.EnvVar{
									{Name: envName, Value: "/custom/path"},
								},
							},
						},
					},
				},
			}

			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			}

			_, err := injectPod(inst, pod, "default")
			require.Error(t, err)
			assert.Contains(t, err.Error(), envName)
			assert.Contains(t, err.Error(), "reserved")
		})
	}
}

func TestInjectPod_NoDeclarativeConfig_NoConfigMount(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// No ConfigMap volume should be added.
	for _, v := range result.Spec.Volumes {
		assert.Nil(t, v.ConfigMap, "should not have any ConfigMap volume")
	}

	// No config file env vars.
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Empty(t, envMap[envOTelConfigFile])
	assert.Empty(t, envMap[envOTelExperimentalConfigFile])
}

func TestInjectPod_DeclarativeConfig_MultipleContainersSameRule(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "my-inst"},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "shared-config",
					Config: v2alpha1.RuleConfig{
						DeclarativeConfig: &v2alpha1.DeclarativeConfig{
							Object: map[string]any{"file_format": "1.0"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app1"},
				{Name: "app2"},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// Only one ConfigMap volume should be added (not duplicated).
	configVolCount := 0
	for _, v := range result.Spec.Volumes {
		if v.ConfigMap != nil {
			configVolCount++
		}
	}
	assert.Equal(t, 1, configVolCount, "should have exactly one ConfigMap volume")

	// Both containers should have the config mount.
	for _, c := range result.Spec.Containers {
		var hasConfigMount bool
		for _, vm := range c.VolumeMounts {
			if vm.MountPath == configMountPath {
				hasConfigMount = true
				break
			}
		}
		assert.True(t, hasConfigMount, "container %s should have config mount", c.Name)
	}
}

func TestInjectPod_DeclarativeConfig_TwoRulesDifferentContainers(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "my-inst"},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{
				{
					Name:     "java-config",
					Selector: v2alpha1.RuleSelector{ContainerNames: []string{"java-app"}},
					Config: v2alpha1.RuleConfig{
						DeclarativeConfig: &v2alpha1.DeclarativeConfig{
							Object: map[string]any{"file_format": "1.0", "lang": "java"},
						},
					},
				},
				{
					Name:     "python-config",
					Selector: v2alpha1.RuleSelector{ContainerNames: []string{"python-app"}},
					Config: v2alpha1.RuleConfig{
						DeclarativeConfig: &v2alpha1.DeclarativeConfig{
							Object: map[string]any{"file_format": "1.0", "lang": "python"},
						},
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "java-app"},
				{Name: "python-app"},
			},
		},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// Two distinct ConfigMap volumes.
	configVols := map[string]string{} // volume name -> ConfigMap name
	for _, v := range result.Spec.Volumes {
		if v.ConfigMap != nil {
			configVols[v.Name] = v.ConfigMap.Name
		}
	}
	assert.Len(t, configVols, 2)
	assert.Equal(t, ConfigMapName("my-inst", "java-config"), configVols[configVolumeName("java-config")])
	assert.Equal(t, ConfigMapName("my-inst", "python-config"), configVols[configVolumeName("python-config")])

	// Each container gets its own config volume mount.
	for _, c := range result.Spec.Containers {
		var mountedVol string
		for _, vm := range c.VolumeMounts {
			if vm.MountPath == configMountPath {
				mountedVol = vm.Name
				break
			}
		}
		require.NotEmpty(t, mountedVol, "container %s should have config mount", c.Name)

		if c.Name == "java-app" {
			assert.Equal(t, configVolumeName("java-config"), mountedVol)
		} else {
			assert.Equal(t, configVolumeName("python-config"), mountedVol)
		}
	}
}

// helpers

func envToMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string)
	for _, e := range envs {
		if e.ValueFrom == nil {
			m[e.Name] = e.Value
		}
	}
	return m
}

func findEnv(envs []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i]
		}
	}
	return nil
}

func countEnv(envs []corev1.EnvVar, name string) int {
	n := 0
	for _, e := range envs {
		if e.Name == name {
			n++
		}
	}
	return n
}

func TestInjectPod_PerLanguageImageVolumes(t *testing.T) {
	javaImg := "java-agent:latest"
	nodejsImg := "nodejs-agent:latest"
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Java:     javaImg,
			NodeJS:   nodejsImg,
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// No init containers — image volumes replace the copy approach.
	assert.Empty(t, result.Spec.InitContainers)

	// Injector image volume + java + nodejs = 3 volumes.
	require.Len(t, result.Spec.Volumes, 3)
	assert.Equal(t, volumeName, result.Spec.Volumes[0].Name)
	require.NotNil(t, result.Spec.Volumes[0].Image)
	assert.Equal(t, "injector:latest", result.Spec.Volumes[0].Image.Reference)
	assert.Equal(t, langVolumeName("java"), result.Spec.Volumes[1].Name)
	require.NotNil(t, result.Spec.Volumes[1].Image)
	assert.Equal(t, javaImg, result.Spec.Volumes[1].Image.Reference)
	assert.Equal(t, langVolumeName("nodejs"), result.Spec.Volumes[2].Name)
	require.NotNil(t, result.Spec.Volumes[2].Image)
	assert.Equal(t, nodejsImg, result.Spec.Volumes[2].Image.Reference)

	// App container has injector mount + per-language mounts.
	require.Len(t, result.Spec.Containers[0].VolumeMounts, 3)
	assert.Equal(t, mountPath, result.Spec.Containers[0].VolumeMounts[0].MountPath)
	assert.Equal(t, langMountPath("java"), result.Spec.Containers[0].VolumeMounts[1].MountPath)
	assert.Equal(t, langMountPath("nodejs"), result.Spec.Containers[0].VolumeMounts[2].MountPath)

	// Lang path env vars point into the per-language mount paths under /autoinstrumentation/.
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Equal(t, langMountPath("java")+"/autoinstrumentation/javaagent.jar", envMap[envJVMAgentPath])
	assert.Equal(t, langMountPath("nodejs")+"/autoinstrumentation/register.js", envMap[envNodejsAgentPath])
	// python/dotnet not configured — should not be set.
	assert.Empty(t, envMap[envPythonAgentPath])
	assert.Empty(t, envMap[envDotnetAgentPath])
}

func TestInjectPod_PerLanguageEnvVarUserOverride(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Java:     "java-agent:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "catch-all",
					Config: v2alpha1.RuleConfig{
						// User provides a custom agent path — should win over operator default.
						Env: []corev1.EnvVar{
							{Name: envJVMAgentPath, Value: "/custom/javaagent.jar"},
						},
					},
				},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	result := mustInjectPod(t, inst, pod, "default")
	envMap := envToMap(result.Spec.Containers[0].Env)
	// User-provided path wins.
	assert.Equal(t, "/custom/javaagent.jar", envMap[envJVMAgentPath])
}

func TestInjectPod_NoPerLanguageImages_NoLangEnvVars(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules:    []v2alpha1.Rule{{Name: "catch-all"}},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// Only the injector image volume — no per-language volumes.
	require.Len(t, result.Spec.Volumes, 1)
	assert.Empty(t, result.Spec.InitContainers)

	// No per-language env vars — otelinject.conf defaults apply.
	envMap := envToMap(result.Spec.Containers[0].Env)
	assert.Empty(t, envMap[envJVMAgentPath])
	assert.Empty(t, envMap[envNodejsAgentPath])
	assert.Empty(t, envMap[envPythonAgentPath])
	assert.Empty(t, envMap[envDotnetAgentPath])
}

func TestInjectPod_PerLanguageImageVolumes_WithDeclarativeConfig(t *testing.T) {
	inst := &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "my-inst"},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Java:     "java-agent:latest",
			NodeJS:   "nodejs-agent:latest",
			Rules: []v2alpha1.Rule{
				{
					Name: "with-config",
					Config: v2alpha1.RuleConfig{
						DeclarativeConfig: &v2alpha1.DeclarativeConfig{
							Object: map[string]any{"file_format": "1.0"},
						},
					},
				},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	result := mustInjectPod(t, inst, pod, "default")

	// 4 volumes: injector image + java + nodejs + declarative config ConfigMap.
	require.Len(t, result.Spec.Volumes, 4)
	assert.Equal(t, volumeName, result.Spec.Volumes[0].Name)
	assert.NotNil(t, result.Spec.Volumes[0].Image, "injector volume should be an image volume")
	assert.Equal(t, langVolumeName("java"), result.Spec.Volumes[1].Name)
	assert.NotNil(t, result.Spec.Volumes[1].Image, "java volume should be an image volume")
	assert.Equal(t, langVolumeName("nodejs"), result.Spec.Volumes[2].Name)
	assert.NotNil(t, result.Spec.Volumes[2].Image, "nodejs volume should be an image volume")
	assert.NotNil(t, result.Spec.Volumes[3].ConfigMap, "config volume should be a ConfigMap volume")

	// Container gets 4 mounts: injector + java + nodejs + config.
	c := result.Spec.Containers[0]
	require.Len(t, c.VolumeMounts, 4)
	assert.Equal(t, mountPath, c.VolumeMounts[0].MountPath)
	assert.Equal(t, langMountPath("java"), c.VolumeMounts[1].MountPath)
	assert.Equal(t, langMountPath("nodejs"), c.VolumeMounts[2].MountPath)
	assert.Equal(t, configMountPath, c.VolumeMounts[3].MountPath)

	// All mounts are read-only.
	for _, vm := range c.VolumeMounts {
		assert.True(t, vm.ReadOnly, "mount %s should be read-only", vm.Name)
	}

	// No mount path conflicts: /otel, /otel-java, /otel-nodejs, /otel-config are all distinct.
	paths := map[string]bool{}
	for _, vm := range c.VolumeMounts {
		assert.False(t, paths[vm.MountPath], "duplicate mount path: %s", vm.MountPath)
		paths[vm.MountPath] = true
	}

	// Env vars: lang agent paths + config file paths + core injector vars.
	envMap := envToMap(c.Env)
	assert.Equal(t, langMountPath("java")+"/autoinstrumentation/javaagent.jar", envMap[envJVMAgentPath])
	assert.Equal(t, langMountPath("nodejs")+"/autoinstrumentation/register.js", envMap[envNodejsAgentPath])
	assert.Equal(t, otelConfigFilePath, envMap[envOTelConfigFile])
	assert.Equal(t, otelConfigFilePath, envMap[envOTelExperimentalConfigFile])
	assert.Equal(t, ldPreloadPath, envMap[envLDPreload])
}

func TestResolveWorkloadRef_ReplicaSet_DerivesDeployment(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "prod",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "myapp-abc123"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	require.NotNil(t, ref)
	assert.Equal(t, "Deployment", ref.Kind)
	assert.Equal(t, "prod", ref.Namespace)
	assert.Equal(t, "myapp", ref.Name)
}

func TestResolveWorkloadRef_ReplicaSet_NoHash(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "prod",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nohash"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	require.NotNil(t, ref)
	// No dash in name → stays as ReplicaSet (can't derive Deployment).
	assert.Equal(t, "ReplicaSet", ref.Kind)
	assert.Equal(t, "nohash", ref.Name)
}

func TestResolveWorkloadRef_StatefulSet(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "prod",
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "redis"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	require.NotNil(t, ref)
	assert.Equal(t, "StatefulSet", ref.Kind)
	assert.Equal(t, "redis", ref.Name)
}

func TestResolveWorkloadRef_DaemonSet(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "kube-system",
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "fluentd"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	require.NotNil(t, ref)
	assert.Equal(t, "DaemonSet", ref.Kind)
	assert.Equal(t, "kube-system", ref.Namespace)
	assert.Equal(t, "fluentd", ref.Name)
}

func TestResolveWorkloadRef_Job(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "batch",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "cleanup-28450380"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	require.NotNil(t, ref)
	assert.Equal(t, "Job", ref.Kind)
	assert.Equal(t, "cleanup-28450380", ref.Name)
}

func TestResolveWorkloadRef_NoOwner(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone-pod", Namespace: "default"},
	}
	ref := resolveWorkloadRef(pod)
	assert.Nil(t, ref)
}

func TestResolveWorkloadRef_UnknownOwner(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "VirtualMachine", Name: "vm-1"}},
		},
	}
	ref := resolveWorkloadRef(pod)
	assert.Nil(t, ref)
}

func mustInjectPod(t *testing.T, inst *v2alpha1.Instrumentation, pod corev1.Pod, namespace string) corev1.Pod {
	t.Helper()
	result, err := injectPod(inst, pod, namespace)
	require.NoError(t, err)
	return result
}
