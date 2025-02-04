package config

import (
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestCriteriaMatcher_MustMatchAllAttributes(t *testing.T) {
	config := Config{}
	require.NoError(t, yaml.Unmarshal([]byte(`discovery:
 services:
 - k8s_namespace: thens
   k8s_pod_name: thepod
   k8s_deployment_name: thedepl
   k8s_replicaset_name: thers
`), &config))

	tests := []struct {
		name       string
		attributes map[string]string
		want       bool
	}{
		{
			name: "match all",
			attributes: map[string]string{
				"k8s_namespace":       "thens",
				"k8s_pod_name":        "is-thepod",
				"k8s_deployment_name": "thedeployment",
				"k8s_replicaset_name": "thers",
			},
			want: true,
		},
		{
			name: "missing metadata",
			attributes: map[string]string{
				"k8s_namespace":       "thens",
				"k8s_pod_name":        "is-thepod",
				"k8s_replicaset_name": "thers",
			},
			want: false,
		},
		{
			name: "different metadata",
			attributes: map[string]string{
				"k8s_namespace":       "thens",
				"k8s_pod_name":        "is-thepod",
				"k8s_deployment_name": "some-deployment",
				"k8s_replicaset_name": "thers",
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attrs := podAttrs{
				metadata: test.attributes,
			}
			err := config.Validate()
			require.NoError(t, err)
			require.Equal(t, test.want, matchByAttributes(&attrs, &config.Discovery.Services[0]))
		})
	}
}
