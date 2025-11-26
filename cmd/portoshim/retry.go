package main

import (
	"context"
	"time"
)

/*
 * fn - function to call
 * delayFn - function which returns delay in milleseconds
 * count - number of iterations
 */
func retry(ctx context.Context, fn func() error, delayFn func() int, count int) {
	for i := 0; i < count; i++ {
		err := fn()
		if err == nil {
			break
		}
		delay := delayFn()
		DebugLog(ctx, "Lets retry after: %v, %v", delay, err)
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
}
