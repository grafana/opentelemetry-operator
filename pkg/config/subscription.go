// Copyright (c) Saswata Mukherjee (@saswatamcode)
// Licensed under the Apache License 2.0.

package config

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

type Subscription interface {
	Subscribe() (watch.Interface, error)
	Reconcile(object runtime.Object, event watch.EventType)
	IsPodEnabled(pod corev1.Pod) bool
}
