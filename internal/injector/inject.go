// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-telemetry/opentelemetry-operator/apis/v2alpha1"
)

const (
	volumeName = "otel-injector"
	mountPath  = "/otel"

	// Image volumes mount the full image filesystem. The injector and language images
	// place their files under /autoinstrumentation/ inside the image, so after mounting
	// at /otel the files are one level deeper than with the old init container cp approach.
	ldPreloadPath  = "/otel/autoinstrumentation/libotelinject.so"
	configFilePath = "/otel/autoinstrumentation/injector/otelinject.conf"

	configVolumePrefix = "otel-config-"
	configMountPath    = "/otel-config"
	otelConfigFilePath = configMountPath + "/" + configMapDataKey

	envLDPreload          = "LD_PRELOAD"
	envInjectorConfigFile = "OTEL_INJECTOR_CONFIG_FILE"
	envOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	// Both env vars point to the same file. SDKs currently read the experimental
	// name; once declarative config stabilizes, they'll switch to the stable name.
	// Setting both ensures the config works regardless of which SDK version the
	// instrumentation image bundles. Each SDK ignores the var it doesn't recognize.
	envOTelConfigFile             = "OTEL_CONFIG_FILE"
	envOTelExperimentalConfigFile = "OTEL_EXPERIMENTAL_CONFIG_FILE"
	envInjectorK8sNamespace       = "OTEL_INJECTOR_K8S_NAMESPACE_NAME"
	envInjectorK8sPodName         = "OTEL_INJECTOR_K8S_POD_NAME"
	envInjectorK8sPodUID          = "OTEL_INJECTOR_K8S_POD_UID"
	envInjectorK8sContainerName   = "OTEL_INJECTOR_K8S_CONTAINER_NAME"
	envInjectorServiceName        = "OTEL_INJECTOR_SERVICE_NAME"
	envInjectorServiceNamespace   = "OTEL_INJECTOR_SERVICE_NAMESPACE"

	// Per-language agent path env var names (override the otelinject.conf defaults).
	// These are NOT reserved — users can set them in config.env to point to custom agent locations.
	envJVMAgentPath    = "JVM_AUTO_INSTRUMENTATION_AGENT_PATH"
	envNodejsAgentPath = "NODEJS_AUTO_INSTRUMENTATION_AGENT_PATH"
	envPythonAgentPath = "PYTHON_AUTO_INSTRUMENTATION_AGENT_PATH_PREFIX"
	envDotnetAgentPath = "DOTNET_AUTO_INSTRUMENTATION_AGENT_PATH_PREFIX"

	envNodeIP   = "OTEL_NODE_IP"
	envPodIP    = "OTEL_POD_IP"
	envNodeName = "OTEL_NODE_NAME"

	envInjectorResourceAttributes = "OTEL_INJECTOR_RESOURCE_ATTRIBUTES"
	envInjectorMode               = "OTEL_INJECTOR_MODE"
)

func isAlreadyInjected(pod corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			return true
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == envLDPreload {
				return true
			}
		}
	}
	return false
}

// resolveMode returns the effective instrumentation mode for a rule.
// Rule-level mode overrides the CR-level default; if neither is set,
// InstallUnlessConflict is used.
func resolveMode(crDefault, ruleOverride *v2alpha1.InstrumentationMode) v2alpha1.InstrumentationMode {
	if ruleOverride != nil {
		return *ruleOverride
	}
	if crDefault != nil {
		return *crDefault
	}
	return v2alpha1.InstrumentationModeInstallUnlessConflict
}

func injectPod(inst *v2alpha1.Instrumentation, pod corev1.Pod, namespace string) (corev1.Pod, error) {
	// Validate all rules up front before mutating the pod.
	for _, rule := range inst.Spec.Rules {
		if err := validateRuleEnv(rule); err != nil {
			return pod, err
		}
	}

	// Track which config volumes have been added to avoid duplicates when
	// multiple containers match the same rule.
	addedConfigVolumes := map[string]bool{}
	injectedAny := false

	serviceName := deriveServiceName(pod)
	langEnvVars := buildLangEnvVars(inst.Spec)

	// Inject env vars into each app container based on matching rules.
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]

		// Skip containers that already have LD_PRELOAD
		if hasEnv(c.Env, envLDPreload) {
			continue
		}

		rule := matchRule(inst.Spec.Rules, namespace, pod.Labels, c.Name)
		if rule == nil {
			continue
		}

		// Resolve effective mode: rule-level > CR defaults > InstallUnlessConflict.
		mode := resolveMode(inst.Spec.Defaults.Mode, rule.Config.Mode)
		if mode == v2alpha1.InstrumentationModeSkip {
			continue
		}

		// Add image volumes on first match.
		// Each image is mounted read-only at its own path — no copying needed.
		// The injector image mounts at /otel; per-language images mount at /otel-{lang}.
		// Files land under /autoinstrumentation/ inside each mount because image volumes
		// expose the full image filesystem rather than just /autoinstrumentation/.
		if !injectedAny {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					Image: &corev1.ImageVolumeSource{
						Reference:  inst.Spec.Injector,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			})
			pod.Spec.Volumes = append(pod.Spec.Volumes,
				buildLangImageVolumes(inst.Spec)...)
			injectedAny = true
		}

		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: mountPath,
			ReadOnly:  true,
		})
		c.VolumeMounts = append(c.VolumeMounts, buildLangVolumeMounts(inst.Spec)...)

		// Mount declarative config ConfigMap if present.
		if rule.Config.DeclarativeConfig != nil {
			configVolName := configVolumeName(rule.Name)
			cmName := ConfigMapName(inst.Name, rule.Name)

			if !addedConfigVolumes[configVolName] {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name: configVolName,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
						},
					},
				})
				addedConfigVolumes[configVolName] = true
			}

			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      configVolName,
				MountPath: configMountPath,
				ReadOnly:  true,
			})
		}

		envVars := buildEnvVars(rule, c.Name, serviceName, namespace, pod.OwnerReferences, langEnvVars, mode)
		if rule.Config.DeclarativeConfig != nil {
			appendIfNotSet(&envVars, corev1.EnvVar{Name: envOTelConfigFile, Value: otelConfigFilePath})
			appendIfNotSet(&envVars, corev1.EnvVar{Name: envOTelExperimentalConfigFile, Value: otelConfigFilePath})
		}
		c.Env = append(c.Env, envVars...)
	}

	return pod, nil
}

// configVolumeName returns a pod-unique volume name for a rule's ConfigMap.
func configVolumeName(ruleName string) string {
	name := configVolumePrefix + ruleName
	// Volume names must be <= 63 chars and DNS-compatible.
	if len(name) > 63 {
		name = truncateWithHash(name, 63)
	}
	return name
}

// matchRule returns the first matching rule for the given container, or nil.
func matchRule(rules []v2alpha1.Rule, namespace string, podLabels map[string]string, containerName string) *v2alpha1.Rule {
	for i := range rules {
		r := &rules[i]
		if matchesNamespace(r.Selector, namespace) &&
			matchesPodLabels(r.Selector, podLabels) &&
			matchesContainerName(r.Selector, containerName) {
			return r
		}
	}
	return nil
}

// matchesNamespace returns true if the selector's namespace list contains the given namespace.
// When the selector has no explicit namespaces (catch-all), it matches all non-system namespaces.
// The Instrumentation CR is cluster-scoped, so catch-all rules apply broadly.
func matchesNamespace(sel v2alpha1.RuleSelector, namespace string) bool {
	if len(sel.Namespaces) == 0 {
		return !isSystemNamespace(namespace)
	}
	return slices.Contains(sel.Namespaces, namespace)
}

// matchesPodLabels returns true if the pod has all labels specified in the selector (AND semantics).
func matchesPodLabels(sel v2alpha1.RuleSelector, podLabels map[string]string) bool {
	for k, v := range sel.PodLabels {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// matchesContainerName returns true if the selector's container list is empty or contains the name.
func matchesContainerName(sel v2alpha1.RuleSelector, name string) bool {
	if len(sel.ContainerNames) == 0 {
		return true
	}
	return slices.Contains(sel.ContainerNames, name)
}

// validateRuleEnv returns an error if any env var in the rule uses reserved names.
func validateRuleEnv(rule v2alpha1.Rule) error {
	for _, e := range rule.Config.Env {
		if strings.HasPrefix(e.Name, "OTEL_INJECTOR_") {
			return fmt.Errorf("rule %q: env var %q uses reserved OTEL_INJECTOR_ prefix", rule.Name, e.Name)
		}
		if e.Name == envOTelConfigFile || e.Name == envOTelExperimentalConfigFile {
			return fmt.Errorf("rule %q: env var %q is reserved — the operator sets it automatically when declarativeConfig is present", rule.Name, e.Name)
		}
	}
	return nil
}

// buildEnvVars constructs the env vars injected into each instrumented container.
//
// The operator sets OTEL_INJECTOR_* env vars for K8s metadata and service identity.
// The injector binary (LD_PRELOAD) merges these into OTEL_RESOURCE_ATTRIBUTES at
// runtime. See opentelemetry-injector/src/resource_attributes.zig for details.
//
// Resource attribute precedence (highest first):
//   - OTEL_RESOURCE_ATTRIBUTES: user-set keys are always preserved
//   - OTEL_INJECTOR_RESOURCE_ATTRIBUTES: adds keys not already present
//   - OTEL_INJECTOR_* individual vars: adds keys not already present
//
// Service name precedence (highest first):
//   - OTEL_SERVICE_NAME: SDK reads it directly, ignores resource attrs
//   - OTEL_RESOURCE_ATTRIBUTES with service.name: injector preserves it
//   - OTEL_INJECTOR_SERVICE_NAME: operator-derived fallback from owner refs
//
// langVolumeName returns the volume name for a per-language image volume.
func langVolumeName(lang string) string {
	return volumeName + "-" + lang
}

// langMountPath returns the mount path for a per-language image volume.
// Each language gets its own mount path to avoid collisions with the injector volume.
func langMountPath(lang string) string {
	return mountPath + "-" + lang
}

// langSpec defines a per-language instrumentation image and its agent path.
// All three build functions (volumes, mounts, env vars) iterate this shared registry.
type langSpec struct {
	name      string // e.g. "java"
	envVar    string // env var pointing the injector to the agent
	agentPath string // path under /autoinstrumentation/ inside the image
}

var langSpecs = []langSpec{
	{"java", envJVMAgentPath, "javaagent.jar"},
	{"nodejs", envNodejsAgentPath, "register.js"},
	{"python", envPythonAgentPath, "python"},
	{"dotnet", envDotnetAgentPath, "dotnet"},
}

// langImages returns the configured (name, image) pairs from the spec, skipping unconfigured languages.
func langImages(spec v2alpha1.InstrumentationSpec) []struct {
	langSpec
	image string
} {
	images := map[string]string{
		"java": spec.Java, "nodejs": spec.NodeJS,
		"python": spec.Python, "dotnet": spec.DotNet,
	}
	var result []struct {
		langSpec
		image string
	}
	for _, ls := range langSpecs {
		if img := images[ls.name]; img != "" {
			result = append(result, struct {
				langSpec
				image string
			}{ls, img})
		}
	}
	return result
}

// buildLangImageVolumes returns one image volume per configured language.
// Each is mounted at its own path (/otel-{lang}) so they don't conflict with each other
// or the injector volume at /otel.
func buildLangImageVolumes(spec v2alpha1.InstrumentationSpec) []corev1.Volume {
	var volumes []corev1.Volume
	for _, lang := range langImages(spec) {
		volumes = append(volumes, corev1.Volume{
			Name: langVolumeName(lang.name),
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{
					Reference:  lang.image,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		})
	}
	return volumes
}

// buildLangVolumeMounts returns volume mounts for all configured per-language image volumes.
func buildLangVolumeMounts(spec v2alpha1.InstrumentationSpec) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	for _, lang := range langImages(spec) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      langVolumeName(lang.name),
			MountPath: langMountPath(lang.name),
			ReadOnly:  true,
		})
	}
	return mounts
}

// buildLangEnvVars returns env vars that tell the injector where to find each language's agent.
// With image volumes the full image filesystem is mounted, so files are under /autoinstrumentation/
// inside the per-language mount path.
// Users can override any of these in config.env — appendIfNotSet semantics apply.
func buildLangEnvVars(spec v2alpha1.InstrumentationSpec) []corev1.EnvVar {
	var envs []corev1.EnvVar
	for _, lang := range langImages(spec) {
		envs = append(envs, corev1.EnvVar{
			Name:  lang.envVar,
			Value: langMountPath(lang.name) + "/autoinstrumentation/" + lang.agentPath,
		})
	}
	return envs
}

func buildEnvVars(rule *v2alpha1.Rule, containerName, serviceName, namespace string, ownerRefs []metav1.OwnerReference, langEnvVars []corev1.EnvVar, mode v2alpha1.InstrumentationMode) []corev1.EnvVar {
	// User env vars go first so they win over operator defaults (K8s uses first occurrence).
	envs := append([]corev1.EnvVar{}, rule.Config.Env...)

	// Operator defaults — only added if the user hasn't set them.
	appendIfNotSet(&envs, corev1.EnvVar{Name: envOTLPProtocol, Value: "http/protobuf"})
	appendIfNotSet(&envs, corev1.EnvVar{
		Name: envNodeIP,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
		},
	})
	appendIfNotSet(&envs, corev1.EnvVar{
		Name: envPodIP,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		},
	})

	// Per-language agent paths — only added if the user hasn't set them.
	// These tell the injector where to find each language's agent after the init container copy.
	for _, e := range langEnvVars {
		appendIfNotSet(&envs, e)
	}

	// OTEL_INJECTOR_* vars are always set (users can't set these — blocked by validation).
	envs = append(envs,
		corev1.EnvVar{Name: envLDPreload, Value: ldPreloadPath},
		corev1.EnvVar{Name: envInjectorConfigFile, Value: configFilePath},
		corev1.EnvVar{
			Name: envInjectorK8sNamespace,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
		corev1.EnvVar{
			Name: envInjectorK8sPodName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
		corev1.EnvVar{
			Name: envInjectorK8sPodUID,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
			},
		},
		corev1.EnvVar{
			Name: envNodeName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
			},
		},
		corev1.EnvVar{Name: envInjectorK8sContainerName, Value: containerName},
		// Container name is the last fallback per semconv spec.
		corev1.EnvVar{Name: envInjectorServiceName, Value: serviceNameWithFallback(serviceName, containerName)},
		corev1.EnvVar{Name: envInjectorServiceNamespace, Value: namespace},
		corev1.EnvVar{Name: envInjectorResourceAttributes, Value: buildInjectorResourceAttrs(containerName, ownerRefs)},
		corev1.EnvVar{Name: envInjectorMode, Value: string(mode)},
	)

	return envs
}

// buildInjectorResourceAttrs constructs the OTEL_INJECTOR_RESOURCE_ATTRIBUTES value.
// Uses $(...) references for values only known at runtime (resolved by K8s variable expansion).
func buildInjectorResourceAttrs(containerName string, ownerRefs []metav1.OwnerReference) string {
	var attrs []string

	// service.instance.id = namespace.podName.containerName (semconv recommendation for K8s)
	// See https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-serviceinstanceid-should-be-calculated
	attrs = append(attrs, fmt.Sprintf("service.instance.id=$(%s).$(%s).%s",
		envInjectorK8sNamespace, envInjectorK8sPodName, containerName))

	// k8s.node.name from downward API
	attrs = append(attrs, fmt.Sprintf("k8s.node.name=$(%s)", envNodeName))

	// Owner ref resource attributes (k8s.replicaset.name, k8s.statefulset.name, etc.)
	//
	// For ReplicaSet and Job, we also derive the parent name (Deployment/CronJob) by
	// stripping the trailing hash suffix (e.g. "myapp-abc123" → "myapp"). This avoids
	// an API call to look up the ReplicaSet's owner during webhook admission.
	// v1alpha1 does a real API lookup with retry (sdkInjector.addParentResourceLabels),
	// but that adds latency to pod creation. The heuristic covers the standard case
	// where K8s appends a hyphen + hash to the parent name.
	for _, owner := range ownerRefs {
		if attr := ownerKindToResourceAttribute(owner.Kind); attr != "" {
			attrs = append(attrs, fmt.Sprintf("%s=%s", attr, owner.Name))
		}
		if owner.Kind == "ReplicaSet" {
			attrs = append(attrs, fmt.Sprintf("k8s.deployment.name=%s", stripHashSuffix(owner.Name)))
		}
		if owner.Kind == "Job" {
			attrs = append(attrs, fmt.Sprintf("k8s.cronjob.name=%s", stripHashSuffix(owner.Name)))
		}
	}

	return strings.Join(attrs, ",")
}

// stripHashSuffix removes the trailing "-<hash>" appended by K8s to child resource names
// (e.g. ReplicaSet "myapp-abc123" → "myapp", Job "cleanup-28450380" → "cleanup").
// Returns the original name if no hyphen is found.
func stripHashSuffix(name string) string {
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		return name[:idx]
	}
	return name
}

func ownerKindToResourceAttribute(kind string) string {
	switch kind {
	case "ReplicaSet":
		return "k8s.replicaset.name"
	case "Deployment":
		return "k8s.deployment.name"
	case "StatefulSet":
		return "k8s.statefulset.name"
	case "DaemonSet":
		return "k8s.daemonset.name"
	case "Job":
		return "k8s.job.name"
	case "CronJob":
		return "k8s.cronjob.name"
	default:
		return ""
	}
}

// deriveServiceName determines service.name following the OTel semconv K8s attribute spec:
// https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-servicename-should-be-calculated
//
// Precedence (first match wins):
//  1. k8s.deployment.name (derived from ReplicaSet owner by stripping hash suffix)
//  2. k8s.replicaset.name
//  3. k8s.statefulset.name
//  4. k8s.daemonset.name
//  5. k8s.cronjob.name (not reachable — pods don't have CronJob as direct owner)
//  6. k8s.job.name
//  7. k8s.pod.name
//  8. k8s.container.name (applied per-container in buildEnvVars, not here)
func deriveServiceName(pod corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		switch owner.Kind {
		case "ReplicaSet":
			return stripHashSuffix(owner.Name)
		case "StatefulSet", "DaemonSet", "Job":
			return owner.Name
		}
	}
	return pod.Name
}

// resolveWorkloadRef determines the parent workload for a pod by inspecting owner references.
// Uses the same heuristic as deriveServiceName: ReplicaSet → Deployment (strip hash suffix),
// StatefulSet/DaemonSet/Job → direct owner. Returns nil if no recognized owner is found.
func resolveWorkloadRef(pod corev1.Pod) *v2alpha1.WorkloadReference {
	for _, owner := range pod.OwnerReferences {
		switch owner.Kind {
		case "ReplicaSet":
			derived := stripHashSuffix(owner.Name)
			if derived != owner.Name {
				return &v2alpha1.WorkloadReference{Kind: "Deployment", Namespace: pod.Namespace, Name: derived}
			}
			return &v2alpha1.WorkloadReference{Kind: "ReplicaSet", Namespace: pod.Namespace, Name: owner.Name}
		case "StatefulSet", "DaemonSet":
			return &v2alpha1.WorkloadReference{Kind: owner.Kind, Namespace: pod.Namespace, Name: owner.Name}
		case "Job":
			return &v2alpha1.WorkloadReference{Kind: "Job", Namespace: pod.Namespace, Name: owner.Name}
		}
	}
	return nil
}

func hasEnv(envs []corev1.EnvVar, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func serviceNameWithFallback(serviceName, containerName string) string {
	if serviceName != "" {
		return serviceName
	}
	return containerName
}

// appendIfNotSet adds the env var only if no env var with the same name is already present.
func appendIfNotSet(envs *[]corev1.EnvVar, env corev1.EnvVar) {
	if !hasEnv(*envs, env.Name) {
		*envs = append(*envs, env)
	}
}

// truncateWithHash shortens a name to maxLen by keeping a prefix and appending
// a short hash of the full name. This avoids collisions when two long names
// share the same prefix.
func truncateWithHash(name string, maxLen int) string {
	hash := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(hash[:4]) // 8 hex chars
	// prefix + "-" + suffix
	prefixLen := maxLen - len(suffix) - 1
	return name[:prefixLen] + "-" + suffix
}
