package config

import (
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"testing"
)

//func TestCriteriaMatcher_Exclude_Metadata(t *testing.T) {
//	config := Config{}
//	require.NoError(t, yaml.Unmarshal([]byte(`discovery:
// services:
// - k8s_node_name: .
// exclude_services:
// - k8s_node_name: bar
//`), &config))
//
//	matcherFunc, err := CriteriaMatcherProvider(&config)()
//	require.NoError(t, err)
//	discoveredProcesses := make(chan []Event[processAttrs], 10)
//	filteredProcesses := make(chan []Event[ProcessMatch], 10)
//	go matcherFunc(discoveredProcesses, filteredProcesses)
//	defer close(discoveredProcesses)
//
//	// it will filter unmatching processes and return a ProcessMatch for these that match
//	processInfo = func(pp processAttrs) (*ProcessInfo, error) {
//		exePath := map[PID]string{
//			1: "/bin/weird33", 2: "/bin/weird33", 3: "server",
//			4: "/bin/something", 5: "server", 6: "/bin/clientweird99"}[pp.pid]
//		return &ProcessInfo{Pid: int32(pp.pid), ExePath: exePath, OpenPorts: pp.openPorts}, nil
//	}
//	nodeFoo := map[string]string{"k8s_node_name": "foo"}
//	nodeBar := map[string]string{"k8s_node_name": "bar"}
//	discoveredProcesses <- []Event[processAttrs]{
//		{Type: EventCreated, Obj: processAttrs{pid: 1, metadata: nodeFoo}}, // pass
//		{Type: EventDeleted, Obj: processAttrs{pid: 2, metadata: nodeFoo}}, // filter
//		{Type: EventCreated, Obj: processAttrs{pid: 3, metadata: nodeFoo}}, // pass
//		{Type: EventCreated, Obj: processAttrs{pid: 4, metadata: nodeBar}}, // filter (in exclude)
//		{Type: EventDeleted, Obj: processAttrs{pid: 5, metadata: nodeFoo}}, // filter
//		{Type: EventCreated, Obj: processAttrs{pid: 6, metadata: nodeBar}}, // filter (in exclude)
//	}
//
//	matches := testutil.ReadChannel(t, filteredProcesses, 1000*testTimeout)
//	require.Len(t, matches, 2)
//	m := matches[0]
//	assert.Equal(t, EventCreated, m.Type)
//	assert.Equal(t, ProcessInfo{Pid: 1, ExePath: "/bin/weird33"}, *m.Obj.Process)
//	m = matches[1]
//	assert.Equal(t, EventCreated, m.Type)
//	assert.Equal(t, ProcessInfo{Pid: 3, ExePath: "server"}, *m.Obj.Process)
//}

func TestCriteriaMatcher_MustMatchAllAttributes(t *testing.T) {
	config := Config{}
	require.NoError(t, yaml.Unmarshal([]byte(`discovery:
 services:
 - name: all-attributes-must-match
   namespace: foons
   open_ports: 80,8080-8089
   exe_path: foo
   k8s_namespace: thens
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
			attrs := processAttrs{
				metadata: test.attributes,
			}
			require.Equal(t, test.want, matchByAttributes(&attrs, &config.Discovery.Services[0]))
		})
	}
}
