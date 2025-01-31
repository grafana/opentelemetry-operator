package config

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/watch"
	"sync"
)

func Start(context.Context) {
	fmt.Println("Starting config map sync")
}

func RunLoop(ctx context.Context, subscriptions []Subscription) error {
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
	return nil
}
