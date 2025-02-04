package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"regexp"
	"slices"
)

const (
	AttrNamespace       = "k8s_namespace"
	AttrPodName         = "k8s_pod_name"
	AttrDeploymentName  = "k8s_deployment_name"
	AttrReplicaSetName  = "k8s_replicaset_name"
	AttrDaemonSetName   = "k8s_daemonset_name"
	AttrStatefulSetName = "k8s_statefulset_name"
	AttrCronJobName     = "k8s_cronjob_name"
	AttrJobName         = "k8s_job_name"
	// AttrOwnerName would be a generic search criteria that would
	// match against deployment, replicaset, daemonset and statefulset names
	AttrOwnerName = "k8s_owner_name"
)

// any attribute name not in this set will cause an error during the YAML unmarshalling
var allowedAttributeNames = map[string]struct{}{
	AttrNamespace:       {},
	AttrPodName:         {},
	AttrDeploymentName:  {},
	AttrReplicaSetName:  {},
	AttrDaemonSetName:   {},
	AttrStatefulSetName: {},
	AttrCronJobName:     {},
	AttrJobName:         {},
	AttrOwnerName:       {},
}

type Config struct {
	// Discovery configuration
	Discovery DiscoveryConfig `yaml:"discovery"`
}

func (c *Config) Validate() error {
	if err := c.Discovery.Services.Validate(); err != nil {
		return fmt.Errorf("error in services YAML property: %w", err)
	}

	return nil
}

// DiscoveryConfig is the configuration.
type DiscoveryConfig struct {
	// Services selection.
	Services DefinitionCriteria `yaml:"services"`
}

// DefinitionCriteria allows defining a group of services to be instrumented according to a set
// of attributes. If a given executable/service matches multiple of the attributes, the
// earliest defined service will take precedence.
type DefinitionCriteria []Attributes

func (dc DefinitionCriteria) Validate() error {
	// an empty definition criteria is valid
	for i := range dc {
		if len(dc[i].Metadata) == 0 &&
			len(dc[i].PodLabels) == 0 {
			return fmt.Errorf("discovery.services[%d] should define at least one selection criteria", i)
		}
		for k := range dc[i].Metadata {
			if _, ok := allowedAttributeNames[k]; !ok {
				return fmt.Errorf("unknown attribute in discovery.services[%d]: %s", i, k)
			}
		}
	}
	return nil
}

// Attributes that specify a given instrumented service.
// Each instance has to define either the OpenPorts or Path property, or both. These are used to match
// a given executable. If both OpenPorts and Path are defined, the inspected executable must fulfill both
// properties.
type Attributes struct {
	// Name will define a name for the matching service. If unset, it will take the name of the executable process
	Name string `yaml:"name"`
	// Namespace will define a namespace for the matching service. If unset, it will be left empty.
	Namespace string `yaml:"namespace"`

	// Metadata stores other attributes, such as Kubernetes object metadata
	Metadata map[string]*RegexpAttr `yaml:",inline"`

	// PodLabels allows matching against the labels of a pod
	PodLabels map[string]*RegexpAttr `yaml:"k8s_pod_labels"`
}

// RegexpAttr stores a regular expression representing an executable file path.
type RegexpAttr struct {
	re *regexp.Regexp
}

func NewPathRegexp(re *regexp.Regexp) RegexpAttr {
	return RegexpAttr{re: re}
}

func (p *RegexpAttr) IsSet() bool {
	return p.re != nil
}

func (p *RegexpAttr) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("RegexpAttr: unexpected YAML node kind %d", value.Kind)
	}
	if len(value.Value) == 0 {
		p.re = nil
		return nil
	}
	re, err := regexp.Compile(value.Value)
	if err != nil {
		return fmt.Errorf("invalid regular expression in node %s: %w", value.Tag, err)
	}
	p.re = re
	return nil
}

func (p *RegexpAttr) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		p.re = nil
		return nil
	}
	re, err := regexp.Compile(string(text))
	if err != nil {
		return fmt.Errorf("invalid regular expression %q: %w", string(text), err)
	}
	p.re = re
	return nil
}

func (p *RegexpAttr) MatchString(input string) bool {
	// no regexp means "empty regexp", so anything will match it
	if p.re == nil {
		return true
	}
	return p.re.MatchString(input)
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
