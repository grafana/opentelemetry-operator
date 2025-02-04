package config

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.25.0"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
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
