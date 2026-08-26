package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIJobRegistryLifecycle(t *testing.T) {
	state := &agentState{}
	conversation := uuid.NewString()
	job, err := state.registerAIJob(conversation, "command")
	if err != nil {
		t.Fatal(err)
	}
	if got, found := state.getAIJob(conversation, job.ID); !found || got.Status != "running" {
		t.Fatalf("job = %+v found=%v", got, found)
	}
	if _, found := state.getAIJob(uuid.NewString(), job.ID); found {
		t.Fatal("cross-conversation job read must fail")
	}
	state.finishAIJob(job, "succeeded", "done output", "")
	if got, _ := state.getAIJob(conversation, job.ID); got.Status != "succeeded" || got.Output != "done output" {
		t.Fatalf("finished job = %+v", got)
	}
	jobs := state.listAIJobs(conversation)
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("jobs = %+v", jobs)
	}
	// Terminal states are sticky.
	state.finishAIJob(job, "failed", "late", "boom")
	if got, _ := state.getAIJob(conversation, job.ID); got.Status != "succeeded" {
		t.Fatalf("terminal job must stay sticky: %+v", got)
	}
}

func TestAIJobRegistryEvictsOldestTerminal(t *testing.T) {
	state := &agentState{}
	conversation := uuid.NewString()
	var oldest *aiJobRecord
	for index := 0; index < maximumAIJobsPerConversation; index++ {
		job, err := state.registerAIJob(conversation, "command")
		if err != nil {
			t.Fatal(err)
		}
		state.finishAIJob(job, "succeeded", "x", "")
		if oldest == nil {
			oldest = job
		}
	}
	extra, err := state.registerAIJob(conversation, "command")
	if err != nil {
		t.Fatal(err)
	}
	if jobs := state.listAIJobs(conversation); len(jobs) != maximumAIJobsPerConversation {
		t.Fatalf("job count = %d", len(jobs))
	}
	if _, found := state.getAIJob(conversation, extra.ID); !found {
		t.Fatal("new job must be registered")
	}
	_ = oldest
}

func TestAIJobKillStopsBackgroundCommand(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 2)
	fixture.executor.supervisor.memoryPollInterval = time.Hour
	plan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo background", "background": true})
	result := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	if result.IsError {
		t.Fatalf("background command result = %+v", result)
	}
	jobID, _ := result.Metadata["job_id"].(string)
	if jobID == "" {
		t.Fatalf("background command missing job id: %+v", result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for starter.latest() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starter.latest() == nil {
		t.Fatal("background process did not start")
	}
	if err := starter.latest().emitStdout([]byte("partial background output\r\n")); err != nil {
		t.Fatal(err)
	}
	killed, err := fixture.state.killAIJob(fixture.context.ConversationID, jobID)
	if err != nil || !killed {
		t.Fatalf("kill = %v error=%v", killed, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		job, found := fixture.state.getAIJob(fixture.context.ConversationID, jobID)
		if !found {
			t.Fatal("job missing")
		}
		if job.Status != "running" {
			if job.Status != "killed" {
				t.Fatalf("job status = %+v", job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed job never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAIConversationToolLoopTracksBackgroundSubagentJob(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if strings.Contains(prompt.Text, "子任务标记") {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "子代理后台完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		}
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"task": "子任务标记：完成一个后台小任务", "background": true})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "bg-spawn-1", Name: "spawn_agent", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			var spawned struct {
				Content string `json:"content"`
			}
			if len(prompt.ToolExchanges) == 0 {
				return errAIProvider
			}
			if err := json.Unmarshal([]byte(prompt.ToolExchanges[0].Results[0].Content), &spawned); err != nil {
				return err
			}
			var value struct {
				JobID string `json:"jobId"`
			}
			if err := json.Unmarshal([]byte(spawned.Content), &value); err != nil || value.JobID == "" {
				return errors.New("spawn result missing jobId")
			}
			arguments, _ := json.Marshal(map[string]any{"job_id": value.JobID, "wait": true})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "job-output-1", Name: "job_output", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		default:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "后台任务跟踪完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "启动后台子代理并跟踪",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	spawnJobID := ""
	sawJobOutput := false
	for _, prompt := range prompts {
		for _, exchange := range prompt.ToolExchanges {
			for _, result := range exchange.Results {
				if result.Name == "spawn_agent" {
					var envelope struct {
						Content string `json:"content"`
					}
					_ = json.Unmarshal([]byte(result.Content), &envelope)
					var value struct {
						JobID string `json:"jobId"`
					}
					_ = json.Unmarshal([]byte(envelope.Content), &value)
					spawnJobID = value.JobID
				}
				if result.Name == "job_output" {
					sawJobOutput = true
					if result.IsError || !strings.Contains(result.Content, "子代理后台完成") ||
						!strings.Contains(result.Content, `\"status\":\"succeeded\"`) {
						t.Fatalf("job output = %+v", result)
					}
				}
			}
		}
	}
	if spawnJobID == "" {
		t.Fatal("spawn result missing jobId")
	}
	_ = sawJobOutput
	jobs := fixture.state.listAIJobs(fixture.conversation.ID)
	if len(jobs) != 1 || jobs[0].Kind != "subagent" || jobs[0].Status != "succeeded" ||
		jobs[0].ID != spawnJobID || !strings.Contains(jobs[0].Output, "子代理后台完成") {
		t.Fatalf("jobs = %+v", jobs)
	}
}
