package config

import (
	"context"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sync"
)

func Start(ctx context.Context, logger logr.Logger, clientSet *kubernetes.Clientset) error {
	subscription := ConfigMapSubscription{
		Ctx:       ctx,
		Logger:    logger,
		ClientSet: clientSet,
		Namespace: configMapNamespace,
	}
	var wg sync.WaitGroup

	wg.Add(1)

	watchInterface, err := subscription.Subscribe()
	if err != nil {
		return err
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
	return nil
}
