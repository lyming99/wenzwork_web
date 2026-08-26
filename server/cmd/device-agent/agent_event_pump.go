package main

import (
	"context"
	"sync"
	"time"
)

// agentEventPump reads only committed journal entries. It never runs inside a
// business transaction and never waits for encryption or Relay socket writes.
type agentEventPump struct {
	store *businessStore
	hub   *agentEventHub

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	once   sync.Once
}

func newAgentEventPump(store *businessStore, hub *agentEventHub) *agentEventPump {
	ctx, cancel := context.WithCancel(context.Background())
	pump := &agentEventPump{
		store: store, hub: hub, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), wake: make(chan struct{}, 1),
	}
	if store != nil {
		store.setAgentEventWake(pump.notify)
	}
	return pump
}

func (pump *agentEventPump) start() {
	if pump == nil || pump.store == nil || pump.hub == nil {
		return
	}
	pump.once.Do(func() { go pump.run() })
}

func (pump *agentEventPump) run() {
	defer close(pump.done)
	const safetyReconcileInterval = 30 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-pump.ctx.Done():
			return
		case <-pump.wake:
		case <-timer.C:
		}
		pump.syncOnce()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(safetyReconcileInterval)
	}
}

func (pump *agentEventPump) notify() {
	if pump == nil {
		return
	}
	select {
	case pump.wake <- struct{}{}:
	default:
	}
}

func (pump *agentEventPump) syncOnce() {
	if pump == nil || pump.store == nil || pump.hub == nil {
		return
	}
	// appendAgentEvent notifies before the surrounding SQL transaction commits.
	// Every production writer retains this mutex through Commit, so acquiring it
	// here guarantees that only durable rows are published.
	pump.store.mu.Lock()
	defer pump.store.mu.Unlock()
	for _, projectID := range pump.hub.activeProjects() {
		after := pump.hub.publishedSequence(projectID)
		info, err := pump.store.agentEventStreamInfo(pump.ctx, projectID)
		if err != nil || info.HighWatermark <= after {
			continue
		}
		events, err := pump.store.listAgentEvents(pump.ctx, projectID, after, info.HighWatermark)
		if err != nil {
			continue
		}
		for _, event := range events {
			pump.hub.publish(event)
		}
	}
}

func (pump *agentEventPump) close() {
	if pump == nil {
		return
	}
	if pump.store != nil {
		pump.store.setAgentEventWake(nil)
	}
	pump.cancel()
	select {
	case <-pump.done:
	case <-time.After(time.Second):
	}
}
