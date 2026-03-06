// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

func newRollbackReconciler(fakeClock *clocktesting.FakeClock, objs ...client.Object) (*RollbackReconciler, client.Client) {
	s := newScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)

	cli := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v2alpha1.Instrumentation{}).
		Build()

	r := NewRollbackReconciler(cli, s, logr.Discard(), nil)
	r.clock = fakeClock
	return r, cli
}

func testInstrumentation(generation int64) *v2alpha1.Instrumentation {
	return &v2alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-inst",
			Generation: generation,
		},
		Spec: v2alpha1.InstrumentationSpec{
			Injector: "injector:latest",
			Rules: []v2alpha1.Rule{{
				Name: "catch-all",
			}},
		},
	}
}

func testNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
}

func testPod(crashReason string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: "myapp-abc123",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Env: []corev1.EnvVar{
					{Name: envLDPreload, Value: ldPreloadPath},
				},
			}},
		},
	}
	if crashReason != "" {
		pod.Status = corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: crashReason,
					},
				},
			}},
		}
	}
	return pod
}

func testDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
		},
	}
}

func TestIsRelevantPod_WithLDPreload(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Env:  []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}},
			}},
		},
	}
	assert.True(t, isRelevantPod(pod))
}

func TestIsRelevantPod_WithCrashLoopBackOff(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	assert.True(t, isRelevantPod(pod))
}

func TestIsRelevantPod_WithImagePullBackOff(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	assert.True(t, isRelevantPod(pod))
}

func TestIsRelevantPod_Unrelated(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	assert.False(t, isRelevantPod(pod))
}

func TestIsRelevantPod_DifferentLDPreload(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Env:  []corev1.EnvVar{{Name: envLDPreload, Value: "/some/other/lib.so"}},
			}},
		},
	}
	assert.False(t, isRelevantPod(pod))
}

func TestResolveRollbackConfig_Defaults(t *testing.T) {
	rc := resolveRollbackConfig(nil)
	assert.True(t, rc.enabled)
	assert.Equal(t, 5*time.Minute, rc.graceTime)
	assert.Equal(t, 1*time.Hour, rc.stabilityWindow)
}

func TestResolveRollbackConfig_CustomValues(t *testing.T) {
	enabled := false
	grace := metav1.Duration{Duration: 10 * time.Minute}
	stability := metav1.Duration{Duration: 2 * time.Hour}
	rc := resolveRollbackConfig(&v2alpha1.RollbackConfig{
		Enabled:         &enabled,
		GraceTime:       &grace,
		StabilityWindow: &stability,
	})
	assert.False(t, rc.enabled)
	assert.Equal(t, 10*time.Minute, rc.graceTime)
	assert.Equal(t, 2*time.Hour, rc.stabilityWindow)
}

func TestRollback_HealthyPods_InventoryPopulated(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	ns := testNamespace()
	pod := testPod("")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Check status was updated with the workload.
	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	assert.Equal(t, "Deployment", updated.Status.InstrumentedWorkloads[0].WorkloadRef.Kind)
	assert.Equal(t, "myapp", updated.Status.InstrumentedWorkloads[0].WorkloadRef.Name)
	assert.Nil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
}

func TestRollback_CrashPastGracePeriod_TriggersRollback(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// 6 minutes past instrumentation — past the 5m grace period.
	now := instrumentedAt.Add(6 * time.Minute)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := testPod("CrashLoopBackOff")
	deploy := testDeployment("myapp", "default")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod, deploy)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	require.NotNil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
	assert.Equal(t, "CrashLoopBackOff", updated.Status.InstrumentedWorkloads[0].Rollback.Reason)

	// Verify deployment was patched with restart annotation.
	var updatedDeploy appsv1.Deployment
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "myapp"}, &updatedDeploy))
	assert.Contains(t, updatedDeploy.Spec.Template.Annotations, "kubectl.kubernetes.io/restartedAt")
}

func TestRollback_CrashWithinGracePeriod_Requeues(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// 3 minutes past instrumentation — within the 5m grace period.
	now := instrumentedAt.Add(3 * time.Minute)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := testPod("CrashLoopBackOff")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, result.RequeueAfter)

	// No rollback yet.
	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	assert.Nil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
}

func TestRollback_CrashPastStabilityWindow_Ignored(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// 2 hours past instrumentation — past the 1h stability window.
	now := instrumentedAt.Add(2 * time.Hour)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := testPod("CrashLoopBackOff")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	assert.Nil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
}

func TestRollback_CRGenerationChanged_ClearsRollback(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(2) // Generation bumped to 2
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(now.Add(-30 * time.Minute)),
		CRGeneration:   1, // Was rolled back at generation 1
		Rollback: &v2alpha1.RollbackInfo{
			Reason:       "CrashLoopBackOff",
			RolledBackAt: metav1.NewTime(now.Add(-20 * time.Minute)),
		},
	}}

	ns := testNamespace()
	pod := testPod("")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	assert.Nil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
	assert.Equal(t, int64(2), updated.Status.InstrumentedWorkloads[0].CRGeneration)
}

func TestRollback_Disabled_DoesNothing(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := clocktesting.NewFakeClock(now)

	disabled := false
	inst := testInstrumentation(1)
	inst.Spec.Defaults.Rollback = &v2alpha1.RollbackConfig{Enabled: &disabled}

	ns := testNamespace()
	pod := testPod("CrashLoopBackOff")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	assert.Empty(t, updated.Status.InstrumentedWorkloads)
}

func TestRollback_ImagePullBackOff_TriggersRollback(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	now := instrumentedAt.Add(6 * time.Minute)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := testPod("ImagePullBackOff")
	deploy := testDeployment("myapp", "default")

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod, deploy)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	require.NotNil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
	assert.Equal(t, "ImagePullBackOff", updated.Status.InstrumentedWorkloads[0].Rollback.Reason)
}

func TestRollback_StatefulSet_TriggersRollback(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	now := instrumentedAt.Add(6 * time.Minute)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "StatefulSet", Namespace: "default", Name: "mydb"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mydb-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "StatefulSet",
				Name: "mydb",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Env:  []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}},
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mydb"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "mydb"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod, sts)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	require.NotNil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
	assert.Equal(t, "StatefulSet", updated.Status.InstrumentedWorkloads[0].WorkloadRef.Kind)

	// Verify StatefulSet was patched with restart annotation.
	var updatedSts appsv1.StatefulSet
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mydb"}, &updatedSts))
	assert.Contains(t, updatedSts.Spec.Template.Annotations, "kubectl.kubernetes.io/restartedAt")
}

func TestRollback_DaemonSet_TriggersRollback(t *testing.T) {
	instrumentedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	now := instrumentedAt.Add(6 * time.Minute)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "DaemonSet", Namespace: "default", Name: "agent"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(instrumentedAt),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "DaemonSet",
				Name: "agent",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Env:  []corev1.EnvVar{{Name: envLDPreload, Value: ldPreloadPath}},
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "agent"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}

	r, cli := newRollbackReconciler(fakeClock, inst, ns, pod, ds)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	require.Len(t, updated.Status.InstrumentedWorkloads, 1)
	require.NotNil(t, updated.Status.InstrumentedWorkloads[0].Rollback)
	assert.Equal(t, "DaemonSet", updated.Status.InstrumentedWorkloads[0].WorkloadRef.Kind)

	// Verify DaemonSet was patched with restart annotation.
	var updatedDs appsv1.DaemonSet
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "agent"}, &updatedDs))
	assert.Contains(t, updatedDs.Spec.Template.Annotations, "kubectl.kubernetes.io/restartedAt")
}

func TestMergeInventory_PreservesExistingEntry(t *testing.T) {
	instrumentedAt := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC))
	ref := v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"}

	existing := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    ref,
		RuleName:       "catch-all",
		InstrumentedAt: instrumentedAt,
		CRGeneration:   1,
	}}
	current := map[v2alpha1.WorkloadReference]string{ref: "catch-all"}

	result := mergeInventory(existing, current, 1, now)
	require.Len(t, result, 1)
	// InstrumentedAt should be preserved from existing, not overwritten with now.
	assert.Equal(t, instrumentedAt, result[0].InstrumentedAt)
	assert.Equal(t, int64(1), result[0].CRGeneration)
}

func TestMergeInventory_AddsNewWorkload(t *testing.T) {
	now := metav1.NewTime(time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC))
	ref := v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "newapp"}

	current := map[v2alpha1.WorkloadReference]string{ref: "catch-all"}

	result := mergeInventory(nil, current, 3, now)
	require.Len(t, result, 1)
	assert.Equal(t, ref, result[0].WorkloadRef)
	assert.Equal(t, now, result[0].InstrumentedAt)
	assert.Equal(t, int64(3), result[0].CRGeneration)
}

func TestMergeInventory_DropsRemovedWorkload(t *testing.T) {
	now := metav1.NewTime(time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC))
	ref := v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "gone"}

	existing := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    ref,
		RuleName:       "catch-all",
		InstrumentedAt: now,
		CRGeneration:   1,
	}}
	// Empty current — workload no longer has instrumented pods.
	current := map[v2alpha1.WorkloadReference]string{}

	result := mergeInventory(existing, current, 1, now)
	assert.Empty(t, result)
}

func TestMergeInventory_PreservesRollbackInfo(t *testing.T) {
	instrumentedAt := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rolledBackAt := metav1.NewTime(time.Date(2025, 1, 1, 0, 10, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC))
	ref := v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"}

	existing := []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    ref,
		RuleName:       "catch-all",
		InstrumentedAt: instrumentedAt,
		CRGeneration:   1,
		Rollback: &v2alpha1.RollbackInfo{
			Reason:       "CrashLoopBackOff",
			RolledBackAt: rolledBackAt,
		},
	}}
	current := map[v2alpha1.WorkloadReference]string{ref: "catch-all"}

	result := mergeInventory(existing, current, 1, now)
	require.Len(t, result, 1)
	require.NotNil(t, result[0].Rollback)
	assert.Equal(t, "CrashLoopBackOff", result[0].Rollback.Reason)
	assert.Equal(t, rolledBackAt, result[0].Rollback.RolledBackAt)
}

func TestMergeInventory_MixedExistingAndNew(t *testing.T) {
	instrumentedAt := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC))
	existingRef := v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "existing"}
	newRef := v2alpha1.WorkloadReference{Kind: "StatefulSet", Namespace: "default", Name: "newdb"}
	removedRef := v2alpha1.WorkloadReference{Kind: "DaemonSet", Namespace: "default", Name: "removed"}

	existing := []v2alpha1.InstrumentedWorkload{
		{WorkloadRef: existingRef, RuleName: "catch-all", InstrumentedAt: instrumentedAt, CRGeneration: 1},
		{WorkloadRef: removedRef, RuleName: "catch-all", InstrumentedAt: instrumentedAt, CRGeneration: 1},
	}
	current := map[v2alpha1.WorkloadReference]string{
		existingRef: "catch-all",
		newRef:      "db-rule",
	}

	result := mergeInventory(existing, current, 2, now)
	require.Len(t, result, 2)

	// Results are sorted by kind/namespace/name.
	// Deployment < StatefulSet
	assert.Equal(t, existingRef, result[0].WorkloadRef)
	assert.Equal(t, instrumentedAt, result[0].InstrumentedAt) // preserved
	assert.Equal(t, newRef, result[1].WorkloadRef)
	assert.Equal(t, now, result[1].InstrumentedAt) // new entry gets now
	assert.Equal(t, int64(2), result[1].CRGeneration)
}

func TestRollback_WorkloadDeleted_DroppedFromInventory(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := clocktesting.NewFakeClock(now)

	inst := testInstrumentation(1)
	inst.Status.InstrumentedWorkloads = []v2alpha1.InstrumentedWorkload{{
		WorkloadRef:    v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "myapp"},
		RuleName:       "catch-all",
		InstrumentedAt: metav1.NewTime(now.Add(-10 * time.Minute)),
		CRGeneration:   1,
	}}

	ns := testNamespace()
	// No pods — workload was deleted.

	r, cli := newRollbackReconciler(fakeClock, inst, ns)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(inst),
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated v2alpha1.Instrumentation
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(inst), &updated))
	assert.Empty(t, updated.Status.InstrumentedWorkloads)
}
