package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// maximumAIParallelToolCalls bounds the concurrent execution pool for
	// parallel-classified tool calls inside one round (DSH maxParallelToolCalls).
	maximumAIParallelToolCalls = 8
	// maximumAIAgentNoProgressRounds is an internal loop-safety invariant, not
	// a model tuning knob. Keeping it beside the repeat reminders prevents an
	// obsolete stored configuration from terminating a healthy agent early.
	maximumAIAgentNoProgressRounds = 8
)

// aiToolExecutionBudget returns the per-call execution budget for a tool. The
// budget bounds only the execute phase; the approval wait runs before it and
// keeps its own defaultAIApprovalTimeout. run_command and terminal_send also
// carry a per-call timeout_seconds argument (1-120s) that stays authoritative
// below this ceiling.
func aiToolExecutionBudget(name string) time.Duration {
	switch name {
	case "run_command", "terminal_send":
		return 120 * time.Second
	case "web_search":
		return 60 * time.Second
	case "web_fetch":
		return 30 * time.Second
	default:
		return 30 * time.Second
	}
}

// aiToolCallRoundGroups splits one round's model-ordered tool calls into
// scheduling groups: contiguous runs of parallel-classified calls share a
// group, every exclusive call stands alone, and group order is model order.
func aiToolCallRoundGroups(calls []aiProviderToolCall) [][]aiProviderToolCall {
	groups := make([][]aiProviderToolCall, 0, len(calls))
	for start := 0; start < len(calls); {
		end := start + 1
		if aiToolCallRunsInParallel(calls[start].Name) {
			for end < len(calls) && aiToolCallRunsInParallel(calls[end].Name) {
				end++
			}
		}
		groups = append(groups, calls[start:end])
		start = end
	}
	return groups
}

// executeAIToolCallRound runs one round's prepared calls in model order with
// DSH barrier semantics: parallel groups execute inside a bounded pool,
// exclusive calls run alone after the pool drains, and results come back in
// model order so the no-progress fingerprint and exchange replay stay stable.
func executeAIToolCallRound(
	ctx context.Context,
	runtime *aiConversationToolRuntime,
	d dispatcher,
	turn aiConversationTurn,
	calls []aiProviderToolCall,
	runs []chatToolRun,
	startedAts []time.Time,
) ([]aiProviderToolResult, error) {
	results := make([]aiProviderToolResult, len(calls))
	offset := 0
	for _, group := range aiToolCallRoundGroups(calls) {
		groupResults, err := runAIToolCallGroup(ctx, runtime, d, turn, group, runs[offset:offset+len(group)], startedAts[offset:offset+len(group)])
		if err != nil {
			return results, err
		}
		copy(results[offset:offset+len(group)], groupResults)
		offset += len(group)
	}
	return results, nil
}

// runAIToolCallGroup executes one scheduling group. Single-call groups run
// inline; parallel groups execute concurrently through a bounded slot pool. A
// call cancelled before it starts still runs through the normal path so it
// persists its own cancelled result, matching the sequential semantics.
func runAIToolCallGroup(
	ctx context.Context,
	runtime *aiConversationToolRuntime,
	d dispatcher,
	turn aiConversationTurn,
	calls []aiProviderToolCall,
	runs []chatToolRun,
	startedAts []time.Time,
) ([]aiProviderToolResult, error) {
	results := make([]aiProviderToolResult, len(calls))
	if len(calls) == 1 || !aiToolCallRunsInParallel(calls[0].Name) {
		result, err := runtime.runPreparedCall(ctx, d, turn, calls[0], runs[0], startedAts[0])
		if err != nil {
			if ctx.Err() == nil {
				results[0] = aiProviderToolFailureResult(calls[0], err)
				return results, nil
			}
			return results, err
		}
		results[0] = result
		return results, nil
	}
	slots := make(chan struct{}, maximumAIParallelToolCalls)
	var errMu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	for index := range calls {
		if ctx.Err() != nil {
			result, err := runtime.runPreparedCall(ctx, d, turn, calls[index], runs[index], startedAts[index])
			if err != nil {
				if ctx.Err() == nil {
					results[index] = aiProviderToolFailureResult(calls[index], err)
				} else {
					recordErr(err)
				}
				continue
			}
			results[index] = result
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			result, err := runtime.runPreparedCall(ctx, d, turn, calls[index], runs[index], startedAts[index])
			if err != nil {
				if ctx.Err() == nil {
					results[index] = aiProviderToolFailureResult(calls[index], err)
				} else {
					recordErr(err)
				}
				return
			}
			results[index] = result
		}(index)
	}
	wg.Wait()
	if firstErr != nil {
		return results, firstErr
	}
	return results, nil
}

// aiToolRepeatReminder mirrors DSH repeat-tool-reminder: when the exact same
// tool exchange repeats, inject an advisory reminder at the gentle (3rd) and
// detailed (5th) thresholds so the model can self-correct before the hard
// internal no-progress limit ends the turn.
func aiToolRepeatReminder(calls []aiProviderToolCall, repeatedRounds uint32) string {
	switch repeatedRounds {
	case 3:
		return "你在用完全相同的参数重复相同的工具调用。请先仔细分析上一次调用的结果：如果任务尚未完成，尝试不同的方法或不同的参数，而不是重复相同的调用。"
	case 5:
		var builder strings.Builder
		builder.WriteString("检测到重复的工具调用：\n")
		for index := range calls {
			if index >= 4 {
				builder.WriteString("- ...\n")
				break
			}
			fmt.Fprintf(&builder, "- tool: %s\n", calls[index].Name)
		}
		builder.WriteString("这些重复调用没有取得进展。请不要再以相同参数调用这些工具；检查最新结果并选择不同的操作、不同的参数，或在证据已经充分时结束任务。")
		return builder.String()
	}
	return ""
}
