// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"context"
	"sync"
	"time"
)

type DynamicPoller struct {
	Min     time.Duration
	Max     time.Duration
	Current time.Duration

	// To facilitate on-demand triggers beyond the scheduled interval
	mu              sync.Mutex
	lastTrigger     time.Time
	onDemandTrigger chan chan struct{}
}

func NewDynamicPoller(minInternval, maxInterval time.Duration) *DynamicPoller {
	return &DynamicPoller{
		Min:     minInternval,
		Max:     maxInterval,
		Current: minInternval,
		// We store up at most 1 signal to trigger immediately
		onDemandTrigger: make(chan chan struct{}, 1),
	}
}

var MinimumTriggerTime = time.Second

func (d *DynamicPoller) TriggerNow() {
	select {
	case d.onDemandTrigger <- make(chan struct{}):
	default:
		// There is already one pending
	}
}

func (d *DynamicPoller) Wait(ctx context.Context, didChanges bool) bool {
	// Adaptively tweak our polling interval.
	if didChanges {
		// If the last poll found some useful work to do, speed up our polling interval
		d.Current = max(d.Current-time.Duration(float64(d.Current)*0.3), d.Min)
	} else {
		// Otherwise, slow it down.
		d.Current = min(time.Duration(float64(d.Current)*1.1), d.Max)
	}
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.lastTrigger = time.Now()
	}()
	var onDemand <-chan time.Time
	delay := time.After(d.Current)
	nextOnDemand := d.onDemandTrigger
	for {
		select {
		case <-ctx.Done():
			return false
		case <-delay:
			return true
		case <-onDemand:
			return true
		case <-nextOnDemand:
			// Ensure we only hit this once
			nextOnDemand = nil
			since := time.Since(d.lastTrigger)
			if since > MinimumTriggerTime {
				// Enough time has passed, trigger immediately
				c := make(chan time.Time)
				close(c)
				onDemand = c
			} else {
				// Backoff a bit but trigger within 1s
				onDemand = time.After(MinimumTriggerTime - since)
			}
		}
	}
}
