package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type yamlFile struct {
	Services DefinitionCriteria `yaml:"services"`
}

func TestYAMLParse_PathRegexp(t *testing.T) {
	inputFile := `
services:
  - name: foo
    k8s_node_name: "^abc$"
`
	yf := yamlFile{}
	require.NoError(t, yaml.Unmarshal([]byte(inputFile), &yf))

	require.Len(t, yf.Services, 1)

	attr := yf.Services[0].Metadata["k8s_node_name"]
	assert.True(t, attr.IsSet())
	assert.True(t, attr.MatchString("abc"))
	assert.False(t, attr.MatchString("cabc"))
	assert.False(t, attr.MatchString("abca"))
}

func TestYAMLParse_OtherAttrs(t *testing.T) {
	inputFile := `
services:
  - name: foo
    k8s_namespace: "^aaa$"
    k8s_pod_name: "^abc$"
    k8s_deployment_name: "^bbb$"
    k8s_replicaset_name: "^bbc$"
`
	yf := yamlFile{}
	require.NoError(t, yaml.Unmarshal([]byte(inputFile), &yf))

	require.Len(t, yf.Services, 1)

	other := yf.Services[0].Metadata
	assert.True(t, other["k8s_namespace"].MatchString("aaa"))
	assert.False(t, other["k8s_namespace"].MatchString("aa"))
	assert.True(t, other["k8s_pod_name"].MatchString("abc"))
	assert.False(t, other["k8s_pod_name"].MatchString("aa"))
	assert.True(t, other["k8s_deployment_name"].MatchString("bbb"))
	assert.False(t, other["k8s_deployment_name"].MatchString("aa"))
	assert.True(t, other["k8s_replicaset_name"].MatchString("bbc"))
	assert.False(t, other["k8s_replicaset_name"].MatchString("aa"))
}
