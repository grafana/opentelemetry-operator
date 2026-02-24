// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Device plugin binary that runs as a DaemonSet on each node.
// It copies the Java agent JAR to the host and registers a virtual
// Kubernetes device resource with kubelet.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	dpapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceName   = "instrumentation.opentelemetry.io/java"
	socketName     = "otel-java-instrumentation.sock"
	devicePoolSize = 1024

	// Where agent files are stored on the host.
	hostAgentDir = "/var/otel/java"
	// Where the agent JAR is bundled inside this container image.
	bundledAgentJAR = "/otel-agents/opentelemetry-javaagent.jar"
	// Mount path inside instrumented containers.
	containerAgentDir = "/otel-auto-instrumentation-java"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Phase 1: Copy agent JAR to host directory.
	if err := copyAgentToHost(); err != nil {
		klog.Fatalf("Failed to copy agent JAR to host: %v", err)
	}
	klog.Info("Agent JAR copied to host")

	// Phase 2: Run the device plugin server.
	if err := runDevicePlugin(ctx); err != nil {
		klog.Fatalf("Device plugin failed: %v", err)
	}
}

// copyAgentToHost copies the bundled Java agent JAR to the host directory.
func copyAgentToHost() error {
	if err := os.MkdirAll(hostAgentDir, 0755); err != nil {
		return fmt.Errorf("create host agent dir: %w", err)
	}

	src, err := os.Open(bundledAgentJAR)
	if err != nil {
		return fmt.Errorf("open bundled JAR: %w", err)
	}
	defer src.Close()

	dstPath := filepath.Join(hostAgentDir, "opentelemetry-javaagent.jar")
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create destination JAR: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy JAR: %w", err)
	}

	klog.Infof("Copied %s -> %s", bundledAgentJAR, dstPath)
	return nil
}

// devicePlugin implements the kubelet device plugin gRPC interface.
type devicePlugin struct {
	dpapi.UnimplementedDevicePluginServer
	devices []*dpapi.Device
	stopCh  chan struct{}
}

func newDevicePlugin() *devicePlugin {
	devices := make([]*dpapi.Device, devicePoolSize)
	for i := range devices {
		devices[i] = &dpapi.Device{
			ID:     uuid.New().String(),
			Health: dpapi.Healthy,
		}
	}
	return &devicePlugin{
		devices: devices,
		stopCh:  make(chan struct{}),
	}
}

func (d *devicePlugin) GetDevicePluginOptions(_ context.Context, _ *dpapi.Empty) (*dpapi.DevicePluginOptions, error) {
	return &dpapi.DevicePluginOptions{}, nil
}

func (d *devicePlugin) ListAndWatch(_ *dpapi.Empty, srv dpapi.DevicePlugin_ListAndWatchServer) error {
	klog.Infof("ListAndWatch: advertising %d virtual devices", len(d.devices))
	if err := srv.Send(&dpapi.ListAndWatchResponse{Devices: d.devices}); err != nil {
		return fmt.Errorf("send device list: %w", err)
	}

	<-d.stopCh
	// Send empty list on shutdown to unregister devices.
	_ = srv.Send(&dpapi.ListAndWatchResponse{Devices: []*dpapi.Device{}})
	return nil
}

func (d *devicePlugin) Allocate(_ context.Context, req *dpapi.AllocateRequest) (*dpapi.AllocateResponse, error) {
	resp := &dpapi.AllocateResponse{}
	for _, creq := range req.ContainerRequests {
		klog.V(2).Infof("Allocate: devices=%v", creq.DevicesIds)
		resp.ContainerResponses = append(resp.ContainerResponses, &dpapi.ContainerAllocateResponse{
			Mounts: []*dpapi.Mount{
				{
					ContainerPath: containerAgentDir,
					HostPath:      hostAgentDir,
					ReadOnly:      true,
				},
			},
		})
	}
	return resp, nil
}

func (d *devicePlugin) PreStartContainer(_ context.Context, _ *dpapi.PreStartContainerRequest) (*dpapi.PreStartContainerResponse, error) {
	return &dpapi.PreStartContainerResponse{}, nil
}

func (d *devicePlugin) GetPreferredAllocation(_ context.Context, _ *dpapi.PreferredAllocationRequest) (*dpapi.PreferredAllocationResponse, error) {
	return &dpapi.PreferredAllocationResponse{}, nil
}

func (d *devicePlugin) stop() {
	close(d.stopCh)
}

// runDevicePlugin starts the gRPC server and registers with kubelet.
func runDevicePlugin(ctx context.Context) error {
	socketPath := filepath.Join(dpapi.DevicePluginPath, socketName)

	// Clean up any stale socket.
	os.Remove(socketPath)

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	dp := newDevicePlugin()
	srv := grpc.NewServer()
	dpapi.RegisterDevicePluginServer(srv, dp)

	go func() {
		klog.Infof("Device plugin server listening on %s", socketPath)
		if err := srv.Serve(lis); err != nil {
			klog.Errorf("gRPC server failed: %v", err)
		}
	}()

	// Register with kubelet.
	if err := registerWithKubelet(); err != nil {
		srv.Stop()
		return fmt.Errorf("register with kubelet: %w", err)
	}
	klog.Info("Registered with kubelet")

	// Wait for shutdown signal.
	<-ctx.Done()
	klog.Info("Shutting down device plugin")
	dp.stop()
	srv.GracefulStop()
	return nil
}

// registerWithKubelet registers this device plugin with the kubelet.
func registerWithKubelet() error {
	kubeletSocket := filepath.Join(dpapi.DevicePluginPath, "kubelet.sock")

	conn, err := grpc.NewClient("unix://"+kubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to kubelet: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := dpapi.NewRegistrationClient(conn)
	_, err = client.Register(ctx, &dpapi.RegisterRequest{
		Version:      dpapi.Version,
		Endpoint:     socketName,
		ResourceName: resourceName,
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	return nil
}
