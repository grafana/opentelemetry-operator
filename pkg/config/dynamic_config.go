// Copyright (c) Saswata Mukherjee (@saswatamcode)
// Licensed under the Apache License 2.0.

package config

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

type DynamicConfig interface {
	Subscribe() (watch.Interface, error)
	Reconcile(ctx context.Context, object runtime.Object, event watch.EventType)
	IsPodEnabled(pod v1.Pod) bool
}

type ConfigMapLoader struct {
	Config    Config
	Ctx       context.Context
	Logger    logr.Logger
	ClientSet kubernetes.Interface
	Namespace string

	watcherInterface watch.Interface
}

func Start(ctx context.Context, logger logr.Logger, clientSet *kubernetes.Clientset) (DynamicConfig, error) {
	loader := ConfigMapLoader{
		Ctx:       ctx,
		Logger:    logger,
		ClientSet: clientSet,
		Namespace: configMapNamespace,
	}
	var wg sync.WaitGroup

	wg.Add(1)

	watchInterface, err := loader.Subscribe()
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case e, ok := <-watchInterface.ResultChan():
				if ok && e.Type != watch.Error {
					loader.Reconcile(ctx, e.Object, e.Type)
				}
			case <-ctx.Done():
				wg.Done()
				return
			}
		}
	}()

	wg.Wait()
	return &loader, nil
}

func (c *ConfigMapLoader) Reconcile(ctx context.Context, object runtime.Object, event watch.EventType) {
	configMap := object.(*v1.ConfigMap)
	c.Logger.Info("ConfigMap subscription event", event, "ConfigMap name", configMap.Name)

	switch event {
	case watch.Modified:
		err := c.loadConfig(ctx, configMap)
		if err != nil {
			c.Logger.Error(err, "error loading config")
			return
		}
	}
}

func (c *ConfigMapLoader) Subscribe() (watch.Interface, error) {
	var err error
	c.watcherInterface, err = c.ClientSet.CoreV1().ConfigMaps(c.Namespace).Watch(c.Ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", configMapName),
	})
	if err != nil {
		return nil, err
	}

	return c.watcherInterface, nil
}

func (c *ConfigMapLoader) IsPodEnabled(pod v1.Pod) bool {
	attrs := getPodAttrs(pod)
	for _, service := range c.Config.Discovery.Services {
		if matchByAttributes(&attrs, &service) {
			return true
		}
	}
	return false
}

func (c *ConfigMapLoader) loadConfig(ctx context.Context, configMap *v1.ConfigMap) error {
	s := configMap.Data[configMapKey]
	newConfig := Config{}
	err := yaml.Unmarshal([]byte(s), &newConfig)
	if err != nil {
		return fmt.Errorf("error unmarshaling YAML: %w", err)
	}
	c.Logger.Info("loaded config")
	err = newConfig.Validate()
	if err != nil {
		return fmt.Errorf("error validating config: %w", err)
	}

	oldConfig := c.Config
	remove, add := diff(oldConfig, newConfig)

	c.restartAll(ctx, remove)
	c.restartAll(ctx, add)

	c.Config = newConfig

	return err
}

func (c *ConfigMapLoader) restartAll(ctx context.Context, criteria DefinitionCriteria) {
	for _, criterion := range criteria {
		err := restartObjectWithAttributes(ctx, c.ClientSet, &criterion)
		if err != nil {
			c.Logger.Error(err, "error restarting object")
		}
	}
}
