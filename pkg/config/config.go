package config

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sync"
)

const (
	configMapKey       = "config.yaml"
	configMapNamespace = "default"
	configMapName      = "auto-instrumentation-config"
)

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

type DynamicConfig interface {
	Subscribe() (watch.Interface, error)
	Reconcile(object runtime.Object, event watch.EventType)
	IsPodEnabled(pod corev1.Pod) bool
}

func Start(ctx context.Context, logger logr.Logger, clientSet *kubernetes.Clientset) (DynamicConfig, error) {
	subscription := ConfigMapLoader{
		Ctx:       ctx,
		Logger:    logger,
		ClientSet: clientSet,
		Namespace: configMapNamespace,
	}
	var wg sync.WaitGroup

	wg.Add(1)

	watchInterface, err := subscription.Subscribe()
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case e, ok := <-watchInterface.ResultChan():
				if ok && e.Type != watch.Error {
					subscription.Reconcile(e.Object, e.Type)
				}
			case <-ctx.Done():
				wg.Done()
				return
			}
		}
	}()

	wg.Wait()
	return &subscription, nil
}
