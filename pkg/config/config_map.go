package config

import (
	"context"
	"github.com/go-logr/logr"
	"github.com/open-telemetry/opentelemetry-operator/internal/config"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
	"time"
)

func Start(ctx context.Context, logger logr.Logger, client client.Client, cfg config.Config, clientset *kubernetes.Clientset) {
	//clientset, err := kubernetes.NewForConfig(cfg.)
	//	if err != nil {
	//		return &Watcher{}, err
	//	}

	subscription := ConfigMapSubscription{
		Ctx:             ctx,
		Logger:          logger,
		ClientSet:       clientset,
		Namespace:       "default",
		RefreshInterval: 1 * time.Second,
	}
	var subscriptions = []Subscription{&subscription}
	var wg sync.WaitGroup

	for i := range subscriptions {
		wg.Add(1)

		go func(subscription Subscription) error {
			watchInterface, err := subscription.Subscribe()
			if err != nil {
				return err
			}
			for {
				select {
				case e, ok := <-watchInterface.ResultChan():
					if ok && e.Type != watch.Error {
						subscription.Reconcile(e.Object, e.Type)
					}
				case <-ctx.Done():
					wg.Done()
					return nil
				}
			}

		}(subscriptions[i]) //nolint
	}

	wg.Wait()
}
