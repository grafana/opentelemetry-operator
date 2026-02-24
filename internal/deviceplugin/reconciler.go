// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"
)

const (
	daemonSetName      = "otel-device-plugin"
	componentLabel     = "app.kubernetes.io/component"
	componentValue     = "device-plugin"
	managedByLabel     = "app.kubernetes.io/managed-by"
	managedByValue     = "opentelemetry-operator"
	nameLabel          = "app.kubernetes.io/name"
	nameValue          = "otel-device-plugin"
	priorityClassName  = "system-node-critical"
	devicePluginSocket = "/var/lib/kubelet/device-plugins"
	otelAgentsPath     = "/var/otel/java"
)

// DevicePluginReconciler watches Instrumentation CRs and ensures the device-plugin
// DaemonSet exists when any Instrumentation CR exists in the cluster.
type DevicePluginReconciler struct {
	client.Client
	scheme          *runtime.Scheme
	logger          logr.Logger
	operatorNS      string
	devicePluginImg string
}

// NewReconciler creates a new DevicePluginReconciler.
func NewReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	logger logr.Logger,
	operatorNS string,
	devicePluginImg string,
) *DevicePluginReconciler {
	return &DevicePluginReconciler{
		Client:          client,
		scheme:          scheme,
		logger:          logger.WithName("device-plugin-reconciler"),
		operatorNS:      operatorNS,
		devicePluginImg: devicePluginImg,
	}
}

func (r *DevicePluginReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	log := r.logger

	// List all Instrumentation CRs across all namespaces.
	var instList v1alpha1.InstrumentationList
	if err := r.List(ctx, &instList); err != nil {
		log.Error(err, "failed to list Instrumentation CRs")
		return reconcile.Result{}, err
	}

	needsDaemonSet := len(instList.Items) > 0

	if needsDaemonSet {
		log.V(1).Info("Instrumentation CRs found, ensuring device-plugin DaemonSet exists", "count", len(instList.Items))
		if err := r.ensureResources(ctx); err != nil {
			return reconcile.Result{}, err
		}
	} else {
		log.V(1).Info("no Instrumentation CRs found, cleaning up device-plugin DaemonSet")
		if err := r.cleanupResources(ctx); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

func (r *DevicePluginReconciler) ensureResources(ctx context.Context) error {
	if err := r.ensureServiceAccount(ctx); err != nil {
		return err
	}
	return r.ensureDaemonSet(ctx)
}

func (r *DevicePluginReconciler) ensureServiceAccount(ctx context.Context) error {
	desired := r.buildServiceAccount()
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	_, err := ctrl.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		return nil
	})
	if err != nil {
		r.logger.Error(err, "failed to create or update ServiceAccount")
	}
	return err
}

func (r *DevicePluginReconciler) ensureDaemonSet(ctx context.Context) error {
	desired := r.buildDaemonSet()
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	op, err := ctrl.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Spec = desired.Spec
		return nil
	})
	if err != nil {
		r.logger.Error(err, "failed to create or update DaemonSet")
		return err
	}
	r.logger.V(1).Info("DaemonSet reconciled", "operation", op)
	return nil
}

func (r *DevicePluginReconciler) cleanupResources(ctx context.Context) error {
	// Delete DaemonSet if it exists.
	ds := &appsv1.DaemonSet{}
	dsKey := types.NamespacedName{Name: daemonSetName, Namespace: r.operatorNS}
	if err := r.Get(ctx, dsKey, ds); err == nil {
		if delErr := r.Delete(ctx, ds); delErr != nil && !errors.IsNotFound(delErr) {
			r.logger.Error(delErr, "failed to delete DaemonSet")
			return delErr
		}
		r.logger.Info("deleted device-plugin DaemonSet")
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Delete ServiceAccount if it exists.
	sa := &corev1.ServiceAccount{}
	saKey := types.NamespacedName{Name: daemonSetName, Namespace: r.operatorNS}
	if err := r.Get(ctx, saKey, sa); err == nil {
		if delErr := r.Delete(ctx, sa); delErr != nil && !errors.IsNotFound(delErr) {
			r.logger.Error(delErr, "failed to delete ServiceAccount")
			return delErr
		}
		r.logger.Info("deleted device-plugin ServiceAccount")
	} else if !errors.IsNotFound(err) {
		return err
	}

	return nil
}

func (r *DevicePluginReconciler) labels() map[string]string {
	return map[string]string{
		nameLabel:      nameValue,
		componentLabel: componentValue,
		managedByLabel: managedByValue,
	}
}

func (r *DevicePluginReconciler) buildServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: r.operatorNS,
			Labels:    r.labels(),
		},
	}
}

func (r *DevicePluginReconciler) buildDaemonSet() *appsv1.DaemonSet {
	labels := r.labels()
	selectorLabels := map[string]string{
		nameLabel: nameValue,
	}
	readOnly := true
	allowEscalation := false
	rootUser := int64(0)
	nonRootUser := int64(65532)

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: r.operatorNS,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: daemonSetName,
					PriorityClassName:  priorityClassName,
					InitContainers: []corev1.Container{
						{
							Name:    "init-permissions",
							Image:   "busybox:1.36",
							Command: []string{"sh", "-c", fmt.Sprintf("chown %d:%d %s", nonRootUser, nonRootUser, otelAgentsPath)},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "otel-agents",
									MountPath: otelAgentsPath,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsUser: &rootUser,
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "device-plugin",
							Image:           r.devicePluginImg,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "device-plugin-socket",
									MountPath: devicePluginSocket,
								},
								{
									Name:      "otel-agents",
									MountPath: otelAgentsPath,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem:   &readOnly,
								AllowPrivilegeEscalation: &allowEscalation,
								RunAsUser:                &rootUser,
								RunAsGroup:               &rootUser,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "device-plugin-socket",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: devicePluginSocket,
									Type: hostPathTypePtr(corev1.HostPathDirectory),
								},
							},
						},
						{
							Name: "otel-agents",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: otelAgentsPath,
									Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate),
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
	}
}

func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType {
	return &t
}

// SetupWithManager registers the controller with the manager.
func (r *DevicePluginReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("device-plugin").
		Watches(&v1alpha1.Instrumentation{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				// Always reconcile to the same key — the DaemonSet is cluster-wide.
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{Name: daemonSetName, Namespace: r.operatorNS}},
				}
			},
		)).
		Complete(r)
}

// Ensure the reconciler satisfies the interface.
var _ reconcile.Reconciler = &DevicePluginReconciler{}
