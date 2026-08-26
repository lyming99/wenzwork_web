package main

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAIJobsPerConversation = 32
	maximumAIJobOutputBytes      = 64 << 10
	aiJobWaitPollInterval        = 50 * time.Millisecond
	aiJobWaitMaximum             = 10 * time.Second
)

// aiJobRecord is one background unit of work the model can poll and kill:
// a background command or a background subagent.
type aiJobRecord struct {
	ID                  string    `json:"id"`
	Kind                string    `json:"kind"`
	ConversationID      string    `json:"conversationId"`
	ChildConversationID string    `json:"childConversationId,omitempty"`
	Status              string    `json:"status"`
	Output              string    `json:"output,omitempty"`
	ErrorCode           string    `json:"errorCode,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	cancel              context.CancelFunc
}

func (state *agentState) registerAIJob(conversationID, kind string) (*aiJobRecord, error) {
	if state == nil || uuid.Validate(conversationID) != nil || kind != "command" && kind != "subagent" {
		return nil, errRPCInvalid
	}
	now := time.Now().UTC()
	job := &aiJobRecord{
		ID: uuid.NewString(), Kind: kind, ConversationID: conversationID,
		Status: "running", CreatedAt: now, UpdatedAt: now,
	}
	state.aiJobMu.Lock()
	defer state.aiJobMu.Unlock()
	if state.aiJobs == nil {
		state.aiJobs = make(map[string]*aiJobRecord)
	}
	count := 0
	for _, existing := range state.aiJobs {
		if existing.ConversationID == conversationID {
			count++
		}
	}
	if count >= maximumAIJobsPerConversation {
		// Evict the oldest terminal job; keep running jobs alive.
		var oldest *aiJobRecord
		for _, existing := range state.aiJobs {
			if existing.ConversationID != conversationID || existing.Status == "running" {
				continue
			}
			if oldest == nil || existing.UpdatedAt.Before(oldest.UpdatedAt) ||
				existing.UpdatedAt.Equal(oldest.UpdatedAt) && existing.CreatedAt.Before(oldest.CreatedAt) {
				oldest = existing
			}
		}
		if oldest == nil {
			return nil, errRPCBusy
		}
		delete(state.aiJobs, oldest.ID)
	}
	state.aiJobs[job.ID] = job
	return job, nil
}

func (state *agentState) attachAIJobChild(job *aiJobRecord, childConversationID string) {
	if state == nil || job == nil {
		return
	}
	state.aiJobMu.Lock()
	job.ChildConversationID = childConversationID
	state.aiJobMu.Unlock()
}

// finishAIJob settles a running job. Terminal states are sticky once set by
// kill; natural settlement only updates jobs still running.
func (state *agentState) finishAIJob(job *aiJobRecord, status, output, errorCode string) {
	if state == nil || job == nil {
		return
	}
	state.aiJobMu.Lock()
	defer state.aiJobMu.Unlock()
	if job.Status != "running" && job.Status != "killed" {
		return
	}
	if job.Status == "killed" && status != "succeeded" {
		return
	}
	job.Status = status
	if status == "failed" || status == "killed" {
		job.ErrorCode = errorCode
	}
	if output != "" {
		job.Output = appendAIJobOutput(job.Output, output)
	}
	job.UpdatedAt = time.Now().UTC()
}

func appendAIJobOutput(existing, next string) string {
	combined := existing + next
	if len(combined) <= maximumAIJobOutputBytes {
		return combined
	}
	// Keep the newest tail so long-running logs stay useful.
	start := len(combined) - maximumAIJobOutputBytes
	for start < len(combined) && !utf8.ValidString(combined[start:]) {
		start++
	}
	return combined[start:]
}

func (state *agentState) listAIJobs(conversationID string) []aiJobRecord {
	if state == nil {
		return nil
	}
	state.aiJobMu.Lock()
	defer state.aiJobMu.Unlock()
	jobs := make([]aiJobRecord, 0, len(state.aiJobs))
	for _, job := range state.aiJobs {
		if job.ConversationID == conversationID {
			copy := *job
			copy.cancel = nil
			jobs = append(jobs, copy)
		}
	}
	return jobs
}

func (state *agentState) getAIJob(conversationID, jobID string) (*aiJobRecord, bool) {
	if state == nil {
		return nil, false
	}
	state.aiJobMu.Lock()
	defer state.aiJobMu.Unlock()
	job, found := state.aiJobs[jobID]
	if !found || job.ConversationID != conversationID {
		return nil, false
	}
	copy := *job
	copy.cancel = nil
	return &copy, true
}

// killAIJob stops a running job. Commands cancel their detached execution
// context; subagents cancel the child generation.
func (state *agentState) killAIJob(conversationID, jobID string) (bool, error) {
	if state == nil || uuid.Validate(jobID) != nil {
		return false, errRPCInvalid
	}
	state.aiJobMu.Lock()
	job, found := state.aiJobs[jobID]
	if !found || job.ConversationID != conversationID {
		state.aiJobMu.Unlock()
		return false, errRPCNotFound
	}
	if job.Status != "running" {
		state.aiJobMu.Unlock()
		return false, nil
	}
	job.Status = "killed"
	job.UpdatedAt = time.Now().UTC()
	cancel := job.cancel
	state.aiJobMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true, nil
}

// syncAISubagentJob mirrors a settled child conversation onto its job record.
func (state *agentState) syncAISubagentJob(childConversationID, status, output, errorCode string) {
	if state == nil || childConversationID == "" {
		return
	}
	state.aiJobMu.Lock()
	var job *aiJobRecord
	for _, existing := range state.aiJobs {
		if existing.ChildConversationID == childConversationID {
			job = existing
			break
		}
	}
	state.aiJobMu.Unlock()
	if job == nil {
		return
	}
	jobStatus := status
	switch status {
	case "completed":
		jobStatus = "succeeded"
	case "interrupted":
		jobStatus, errorCode = "killed", "interrupted"
	}
	state.finishAIJob(job, jobStatus, output, errorCode)
}

func (state *agentState) waitAIJob(job *aiJobRecord, timeout time.Duration) *aiJobRecord {
	if state == nil || job == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, found := state.getAIJob(job.ConversationID, job.ID)
		if !found || current.Status != "running" {
			return current
		}
		time.Sleep(aiJobWaitPollInterval)
	}
	current, _ := state.getAIJob(job.ConversationID, job.ID)
	return current
}
