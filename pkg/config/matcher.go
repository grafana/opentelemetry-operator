package config

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.25.0"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"strings"
	"time"
)

type podAttrs struct {
	metadata  map[string]string
	podLabels map[string]string
}

func getPodAttrs(pod v1.Pod) podAttrs {
	ownerName := pod.Name
	if topOwner := topOwner(pod); topOwner != nil {
		ownerName = topOwner.Name
	}

	metadata := map[string]string{
		AttrNamespace: pod.Namespace,
		AttrPodName:   pod.Name,
		AttrOwnerName: ownerName,
	}
	podLabels := pod.Labels

	// add any other owner name (they might be several, e.g. replicaset and deployment)
	for _, owner := range pod.OwnerReferences {
		key := ownerLabelName(owner.Kind)
		if key == "" {
			continue
		}
		metadata[prom(key)] = owner.Name
	}

	return podAttrs{
		metadata:  metadata,
		podLabels: podLabels,
	}
}

func prom(an attribute.Key) string {
	return strings.ReplaceAll(string(an), ".", "_")
}

func ownerLabelName(kind string) attribute.Key {
	switch kind {
	case "Deployment":
		return semconv.K8SDeploymentNameKey
	case "StatefulSet":
		return semconv.K8SStatefulSetNameKey
	case "DaemonSet":
		return semconv.K8SDaemonSetNameKey
	case "ReplicaSet":
		return semconv.K8SReplicaSetNameKey
	case "CronJob":
		return semconv.K8SCronJobNameKey
	case "Job":
		return semconv.K8SJobNameKey
	default:
		return ""
	}
}

func topOwner(pod v1.Pod) *metav1.OwnerReference {
	o := pod.GetOwnerReferences()
	if len(o) == 0 {
		return nil
	}
	return &o[len(o)-1]
}

func matchByAttributes(actual *podAttrs, required *Attributes) bool {
	if required == nil {
		return true
	}
	if actual == nil {
		return false
	}

	// match metadata
	for attrName, criteriaRegexp := range required.Metadata {
		if attrValue, ok := actual.metadata[attrName]; !ok || !criteriaRegexp.MatchString(attrValue) {
			return false
		}
	}

	// match pod labels
	for labelName, criteriaRegexp := range required.PodLabels {
		if actualPodLabelValue, ok := actual.podLabels[labelName]; !ok || !criteriaRegexp.MatchString(actualPodLabelValue) {
			return false
		}
	}
	return true
}

func restartObjectWithAttributes(ctx context.Context, clientSet kubernetes.Interface, required *Attributes) error {
	// pod labels is not compatible with metadata - unless it's namespace
	// is namespace always required? does it need to be a simple string?
	labels := required.PodLabels
	namespace := "default"
	ns := required.Metadata[AttrNamespace]
	if ns != nil {
		namespace = ns.re.String()
	}

	for attrName := range required.Metadata {
		switch attrName {
		case AttrNamespace:
		case AttrPodName:
			continue
		}
		if len(labels) > 0 {
			return errors.New("pod labels is not compatible with metadata")
		}
	}

	for attrName, criteriaRegexp := range required.Metadata {
		switch attrName {
		case AttrNamespace:
		case AttrPodName:
		case AttrDeploymentName:
			err := restartDeployment(ctx, clientSet, namespace, criteriaRegexp)
			if err != nil {
				return err
			}
		case AttrReplicaSetName:
			// same as deployment
		case AttrDaemonSetName:
			// same as deployment
		case AttrStatefulSetName:
			// same as deployment
		case AttrCronJobName:
			// same as deployment
		case AttrJobName:
			// same as deployment
		case AttrOwnerName:
			// same as deployment
		}
	}
	return nil
}

func restartDeployment(ctx context.Context, clientSet kubernetes.Interface, namespace string, criteriaRegexp *RegexpAttr) error {
	list, err := clientSet.AppsV1().Deployments(namespace).List(ctx, listOptions(criteriaRegexp))
	if err != nil {
		return err
	}

	// we don't check if the deployment is already instrumented or not
	// doing so would require fetching add pods of the deployment and checking if they are instrumented

	// how to query pods of a deployment:
	// https://stackoverflow.com/questions/52957227/kubectl-command-to-list-pods-of-a-deployment-in-kubernetes

	// how to see if the pod is currently instrumented:
	// https://github.com/grafana/opentelemetry-operator/blob/4da4f66e0a4d59f0f99a6b6b3fcbd68523e0506c/pkg/instrumentation/helper.go#L46

	for _, item := range list.Items {
		_, err := clientSet.AppsV1().Deployments(namespace).Patch(ctx, item.Name, types.StrategicMergePatchType, restartPatch(), metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func listOptions(criteriaRegexp *RegexpAttr) metav1.ListOptions {
	return metav1.ListOptions{
		// can't use regex - see https://github.com/kubernetes/kubernetes/issues/107053
		FieldSelector: fmt.Sprintf("metadata.name=%s", criteriaRegexp.re.String()),
	}
}

func restartPatch() []byte {
	// see https://stackoverflow.com/questions/61335318/how-to-restart-a-deployment-in-kubernetes-using-go-client

	data := fmt.Sprintf(`{"spec": {"template": {"metadata": {"annotations": {"kubectl.kubernetes.io/restartedAt": "%s"}}}}}`, time.Now().Format("20060102150405"))
	return []byte(data)
}
