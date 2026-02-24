// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"
)

func TestInjectOTELEnvVars_JavaSpecificEnv(t *testing.T) {
	container := &corev1.Container{Name: "app"}
	inst := &v1alpha1.Instrumentation{
		Spec: v1alpha1.InstrumentationSpec{
			Env: []corev1.EnvVar{
				{Name: "OTEL_METRICS_EXPORTER", Value: "none"},
			},
			Java: v1alpha1.Java{
				Env: []corev1.EnvVar{
					{Name: "OTEL_JAVAAGENT_DEBUG", Value: "true"},
					{Name: "OTEL_INSTRUMENTATION_JDBC_ENABLED", Value: "false"},
				},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
	}

	injectOTELEnvVars(container, inst, pod)

	envMap := map[string]string{}
	for _, env := range container.Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}

	// Global env var injected.
	assert.Equal(t, "none", envMap["OTEL_METRICS_EXPORTER"])

	// Java-specific env vars injected.
	assert.Equal(t, "true", envMap["OTEL_JAVAAGENT_DEBUG"])
	assert.Equal(t, "false", envMap["OTEL_INSTRUMENTATION_JDBC_ENABLED"])
}

func TestInjectOTELEnvVars_JavaEnvDoesNotOverride(t *testing.T) {
	container := &corev1.Container{
		Name: "app",
		Env: []corev1.EnvVar{
			{Name: "OTEL_JAVAAGENT_DEBUG", Value: "false"},
		},
	}
	inst := &v1alpha1.Instrumentation{
		Spec: v1alpha1.InstrumentationSpec{
			Java: v1alpha1.Java{
				Env: []corev1.EnvVar{
					{Name: "OTEL_JAVAAGENT_DEBUG", Value: "true"},
				},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
	}

	injectOTELEnvVars(container, inst, pod)

	// User's existing value should be preserved.
	for _, env := range container.Env {
		if env.Name == "OTEL_JAVAAGENT_DEBUG" {
			assert.Equal(t, "false", env.Value)
			return
		}
	}
	t.Fatal("OTEL_JAVAAGENT_DEBUG not found")
}
