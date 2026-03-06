// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func fakeClientWithVersion(serverMinor string, nodes ...corev1.Node) *k8sfake.Clientset {
	objs := make([]k8sruntime.Object, len(nodes))
	for i := range nodes {
		n := nodes[i]
		objs[i] = &n
	}
	cs := k8sfake.NewClientset(objs...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{
		Major: "1",
		Minor: serverMinor,
	}
	return cs
}

func makeNode(name, runtimeVersion string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: runtimeVersion,
			},
		},
	}
}

func TestCheckImageVolumeSupport_Supported(t *testing.T) {
	cs := fakeClientWithVersion("32",
		makeNode("node-1", "containerd://2.1.0"),
		makeNode("node-2", "containerd://2.2.3"),
	)
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Empty(t, reason)
}

func TestCheckImageVolumeSupport_KubeVersionTooOld(t *testing.T) {
	cs := fakeClientWithVersion("31",
		makeNode("node-1", "containerd://2.1.0"),
	)
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Contains(t, reason, "1.31")
	assert.Contains(t, reason, "1.32")
}

func TestCheckImageVolumeSupport_KubeVersionWithPlus(t *testing.T) {
	// Some distributions append a "+" to the minor version (e.g. kind).
	cs := fakeClientWithVersion("35+",
		makeNode("node-1", "containerd://2.1.0"),
	)
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Empty(t, reason, "'+' suffix should be stripped before parsing")
}

func TestCheckImageVolumeSupport_ContainerdVersionTooOld(t *testing.T) {
	cs := fakeClientWithVersion("35",
		makeNode("node-1", "containerd://2.1.0"),
		makeNode("node-2", "containerd://1.7.23"),
	)
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Contains(t, reason, "node-2")
	assert.Contains(t, reason, "1.7")
}

func TestCheckImageVolumeSupport_NotContainerd(t *testing.T) {
	cs := fakeClientWithVersion("35",
		makeNode("node-1", "containerd://2.1.0"),
		makeNode("node-2", "crio://1.30.0"),
	)
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Contains(t, reason, "node-2")
	assert.Contains(t, reason, "crio://1.30.0")
}

func TestCheckImageVolumeSupport_NoNodes(t *testing.T) {
	// Edge case: no nodes at all — treat as supported (nothing to block).
	cs := fakeClientWithVersion("32")
	reason := CheckImageVolumeSupport(context.Background(), logr.Discard(), cs)
	assert.Empty(t, reason)
}

func TestParseContainerdVersion(t *testing.T) {
	tests := []struct {
		runtime        string
		wantContainerd bool
		wantMajor      int
		wantMinor      int
		wantErr        bool
	}{
		{"containerd://2.1.3", true, 2, 1, false},
		{"containerd://1.7.23", true, 1, 7, false},
		{"containerd://2.0.0", true, 2, 0, false},
		{"crio://1.30.0", false, 0, 0, false},
		{"docker://24.0.5", false, 0, 0, false},
		{"containerd://2", true, 0, 0, true}, // missing minor
	}

	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			isContainerd, major, minor, err := parseContainerdVersion(tt.runtime)
			assert.Equal(t, tt.wantContainerd, isContainerd)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				if tt.wantContainerd {
					assert.Equal(t, tt.wantMajor, major)
					assert.Equal(t, tt.wantMinor, minor)
				}
			}
		})
	}
}
