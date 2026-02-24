// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"
)

var (
	testScheme *runtime.Scheme = scheme.Scheme
	testLogger                 = logf.Log.WithName("device-plugin-reconciler-test")
)

func init() {
	utilruntime.Must(v1alpha1.AddToScheme(testScheme))
}

const (
	testNamespace = "opentelemetry-operator-system"
	testImage     = "ghcr.io/open-telemetry/opentelemetry-operator/device-plugin:latest"
)

func newTestReconciler(objs ...runtime.Object) *DevicePluginReconciler {
	clientObjs := make([]runtime.Object, len(objs))
	copy(clientObjs, objs)
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithRuntimeObjects(clientObjs...).
		Build()
	return NewReconciler(fakeClient, testScheme, testLogger, testNamespace, testImage)
}

func TestReconcile_InstrumentationCreated_DaemonSetCreated(t *testing.T) {
	inst := &v1alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-instrumentation",
			Namespace: "default",
		},
	}

	r := newTestReconciler(inst)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: testNamespace},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify DaemonSet was created.
	ds := &appsv1.DaemonSet{}
	err = r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: testNamespace}, ds)
	require.NoError(t, err)
	assert.Equal(t, testImage, ds.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, nameValue, ds.Labels[nameLabel])
	assert.Equal(t, componentValue, ds.Labels[componentLabel])
	assert.Equal(t, managedByValue, ds.Labels[managedByLabel])

	// Verify ServiceAccount was created.
	sa := &corev1.ServiceAccount{}
	err = r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: testNamespace}, sa)
	require.NoError(t, err)
	assert.Equal(t, nameValue, sa.Labels[nameLabel])
}

func TestReconcile_NoInstrumentation_DaemonSetDeleted(t *testing.T) {
	// Pre-create DaemonSet and ServiceAccount as if they were previously created.
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: testNamespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{nameLabel: nameValue},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{nameLabel: nameValue},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "device-plugin", Image: testImage}},
				},
			},
		},
	}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: testNamespace,
		},
	}

	r := newTestReconciler(ds, sa)
	ctx := context.Background()

	// No Instrumentation CRs — should clean up.
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: testNamespace},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify DaemonSet was deleted.
	err = r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: testNamespace}, &appsv1.DaemonSet{})
	assert.True(t, err != nil, "DaemonSet should be deleted")

	// Verify ServiceAccount was deleted.
	err = r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: testNamespace}, &corev1.ServiceAccount{})
	assert.True(t, err != nil, "ServiceAccount should be deleted")
}

func TestReconcile_DaemonSetAlreadyExists_NoError(t *testing.T) {
	inst := &v1alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-instrumentation",
			Namespace: "default",
		},
	}

	r := newTestReconciler(inst)
	ctx := context.Background()

	// First reconcile — creates resources.
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: testNamespace},
	})
	require.NoError(t, err)

	// Second reconcile — should succeed without error (update path).
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: testNamespace},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify DaemonSet still exists.
	ds := &appsv1.DaemonSet{}
	err = r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: testNamespace}, ds)
	require.NoError(t, err)
}

func TestReconcile_NoResources_CleanupNoError(t *testing.T) {
	// No Instrumentation CRs and no DaemonSet — should be a no-op.
	r := newTestReconciler()
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: testNamespace},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}
