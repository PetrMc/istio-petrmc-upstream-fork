// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"testing"
	"time"

	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
)

func TestDynamicPoller(t *testing.T) {
	MinimumTriggerTime = time.Millisecond * 100
	poller := NewDynamicPoller(time.Second*5, time.Minute)
	ctx := test.NewContext(t)
	wait := func() time.Duration {
		t0 := time.Now()
		poller.Wait(ctx, false)
		return time.Since(t0)
	}
	poller.TriggerNow()
	assert.Equal(t, wait() < time.Second*4, true)

	// Next wait should have some backoff but still be less than the full wait period
	poller.TriggerNow()
	waited := wait()
	assert.Equal(t, waited > MinimumTriggerTime/2, true)
	assert.Equal(t, waited < time.Second*4, true)
}

func TestDynamicPollerRace(t *testing.T) {
	MinimumTriggerTime = time.Millisecond * 100
	poller := NewDynamicPoller(time.Millisecond*500, time.Minute)
	ctx := test.NewContext(t)
	wait := func() time.Duration {
		t0 := time.Now()
		poller.Wait(ctx, false)
		return time.Since(t0)
	}
	for i := range 1000 {
		go func() {
			time.Sleep(time.Millisecond * time.Duration(i))
			poller.TriggerNow()
		}()
	}
	wait()
}
