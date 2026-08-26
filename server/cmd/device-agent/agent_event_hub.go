package main

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// agentEventHub is process-local only. Its durable source is agent_event_journal;
// subscribers can always reconnect and replay from their persisted cursor.
type agentEventHub struct {
	mu       sync.Mutex
	projects map[uuid.UUID]*agentEventProjectHub
	closed   bool
}

type agentEventProjectHub struct {
	publishedSequence uint64
	subscribers       map[*agentEventSubscriber]struct{}
}

type agentEventSubscriber struct {
	hub       *agentEventHub
	projectID uuid.UUID
	events    chan agentEventRecord

	mu              sync.Mutex
	closed          bool
	reset           bool
	resetReason     string
	queueBytes      int
	live            bool
	suppressThrough uint64
}

func newAgentEventHub() *agentEventHub {
	return &agentEventHub{projects: make(map[uuid.UUID]*agentEventProjectHub)}
}

func (hub *agentEventHub) subscribe(projectID uuid.UUID) (*agentEventSubscriber, error) {
	if hub == nil || projectID == uuid.Nil {
		return nil, errRPCInvalid
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return nil, context.Canceled
	}
	project := hub.projectLocked(projectID)
	if hub.subscriptionCountLocked() >= maximumAgentEventSubscriptions || len(project.subscribers) >= maximumAgentEventSubscriptionsPerProject {
		return nil, errRPCBusy
	}
	subscriber := &agentEventSubscriber{
		hub: hub, projectID: projectID, events: make(chan agentEventRecord, maximumAgentEventQueueCount),
	}
	project.subscribers[subscriber] = struct{}{}
	return subscriber, nil
}

func (hub *agentEventHub) projectLocked(projectID uuid.UUID) *agentEventProjectHub {
	project := hub.projects[projectID]
	if project == nil {
		project = &agentEventProjectHub{subscribers: make(map[*agentEventSubscriber]struct{})}
		hub.projects[projectID] = project
	}
	return project
}

func (hub *agentEventHub) subscriptionCountLocked() int {
	count := 0
	for _, project := range hub.projects {
		count += len(project.subscribers)
	}
	return count
}

func (hub *agentEventHub) unsubscribe(subscriber *agentEventSubscriber) {
	if hub == nil || subscriber == nil {
		return
	}
	hub.mu.Lock()
	project := hub.projects[subscriber.projectID]
	if project != nil {
		delete(project.subscribers, subscriber)
		if len(project.subscribers) == 0 {
			delete(hub.projects, subscriber.projectID)
		}
	}
	hub.mu.Unlock()
	subscriber.mu.Lock()
	subscriber.closed = true
	subscriber.mu.Unlock()
}

func (subscriber *agentEventSubscriber) close() {
	if subscriber != nil && subscriber.hub != nil {
		subscriber.hub.unsubscribe(subscriber)
	}
}

func (hub *agentEventHub) activeProjects() []uuid.UUID {
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	result := make([]uuid.UUID, 0, len(hub.projects))
	for projectID, project := range hub.projects {
		if len(project.subscribers) > 0 {
			result = append(result, projectID)
		}
	}
	return result
}

func (hub *agentEventHub) publishedSequence(projectID uuid.UUID) uint64 {
	if hub == nil || projectID == uuid.Nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if project := hub.projects[projectID]; project != nil {
		return project.publishedSequence
	}
	return 0
}

func (hub *agentEventHub) publish(event agentEventRecord) {
	if hub == nil || event.ProjectID == uuid.Nil || event.Sequence == 0 {
		return
	}
	hub.mu.Lock()
	project := hub.projects[event.ProjectID]
	if project == nil || event.Sequence <= project.publishedSequence {
		hub.mu.Unlock()
		return
	}
	project.publishedSequence = event.Sequence
	subscribers := make([]*agentEventSubscriber, 0, len(project.subscribers))
	for subscriber := range project.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	hub.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.enqueue(event)
	}
}

func (subscriber *agentEventSubscriber) enqueue(event agentEventRecord) {
	if subscriber == nil {
		return
	}
	bytes := len(event.SafePayloadJSON)
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	if subscriber.closed || subscriber.reset || !subscriber.live || event.Sequence <= subscriber.suppressThrough {
		return
	}
	if len(subscriber.events) >= cap(subscriber.events) || subscriber.queueBytes+bytes > maximumAgentEventQueueBytes {
		subscriber.reset = true
		subscriber.resetReason = "slowConsumer"
		return
	}
	select {
	case subscriber.events <- event:
		subscriber.queueBytes += bytes
	default:
		subscriber.reset = true
		subscriber.resetReason = "slowConsumer"
	}
}

// beginLiveAt installs the boundary captured for a direct journal replay.
// Older entries are deliberately suppressed for this subscriber only; the
// project-wide pump watermark must keep moving for every existing subscriber.
func (subscriber *agentEventSubscriber) beginLiveAt(sequence uint64) {
	if subscriber == nil {
		return
	}
	subscriber.mu.Lock()
	if !subscriber.closed {
		subscriber.suppressThrough = sequence
		subscriber.live = true
	}
	subscriber.mu.Unlock()
}

func (subscriber *agentEventSubscriber) next(ctx context.Context) (agentEventRecord, bool) {
	if subscriber == nil {
		return agentEventRecord{}, false
	}
	select {
	case <-ctx.Done():
		return agentEventRecord{}, false
	case event := <-subscriber.events:
		subscriber.mu.Lock()
		subscriber.queueBytes -= len(event.SafePayloadJSON)
		if subscriber.queueBytes < 0 {
			subscriber.queueBytes = 0
		}
		closed := subscriber.closed
		subscriber.mu.Unlock()
		return event, !closed
	}
}

func (subscriber *agentEventSubscriber) consume(event agentEventRecord) bool {
	if subscriber == nil {
		return false
	}
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	subscriber.queueBytes -= len(event.SafePayloadJSON)
	if subscriber.queueBytes < 0 {
		subscriber.queueBytes = 0
	}
	return !subscriber.closed
}

func (subscriber *agentEventSubscriber) resetReasonValue() string {
	if subscriber == nil {
		return ""
	}
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	if !subscriber.reset {
		return ""
	}
	return subscriber.resetReason
}

func (hub *agentEventHub) close() {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return
	}
	hub.closed = true
	subscribers := make([]*agentEventSubscriber, 0, hub.subscriptionCountLocked())
	for _, project := range hub.projects {
		for subscriber := range project.subscribers {
			subscribers = append(subscribers, subscriber)
		}
	}
	hub.projects = make(map[uuid.UUID]*agentEventProjectHub)
	hub.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.mu.Lock()
		subscriber.closed = true
		subscriber.reset = true
		subscriber.resetReason = "agentShutdown"
		subscriber.mu.Unlock()
	}
}
