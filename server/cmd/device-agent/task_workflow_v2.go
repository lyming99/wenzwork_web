package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumWorkflowV2Nodes       = 128
	maximumWorkflowV2Edges       = 512
	maximumWorkflowV2Identifier  = 128
	maximumWorkflowV2Description = 64 << 10
	maximumWorkflowV2Parallelism = 64
)

var (
	workflowV2NodeTypes       = []string{"start", "task", "finish"}
	workflowV2EdgeTypes       = []string{"onSuccess", "onFailure", "always"}
	workflowV2FailurePolicies = []string{"stopOnFailure", "continueOnFailure"}
	workflowV2NodeStatuses    = []string{"pending", "ready", "running", "succeeded", "failed", "blocked", "cancelled", "skipped"}
)

type workflowV2Revision struct {
	ID                 uuid.UUID        `json:"id"`
	WorkflowTaskID     uuid.UUID        `json:"workflowTaskId"`
	Version            uint64           `json:"version"`
	Description        string           `json:"description"`
	FailurePolicy      string           `json:"failurePolicy"`
	MaximumParallelism uint32           `json:"maximumParallelism"`
	GraphDigest        string           `json:"graphDigest"`
	Nodes              []workflowV2Node `json:"nodes"`
	Edges              []workflowV2Edge `json:"edges"`
	CreatedAt          time.Time        `json:"createdAt"`
}

type workflowV2Node struct {
	ID                 string             `json:"id"`
	Type               string             `json:"type"`
	TaskDefinitionID   *uuid.UUID         `json:"taskDefinitionId,omitempty"`
	TaskDefinition     *taskV2Definition  `json:"taskDefinition,omitempty"`
	SourceTaskID       *uuid.UUID         `json:"sourceTaskId,omitempty"`
	SourceTaskRevision *uint64            `json:"sourceTaskRevision,omitempty"`
	Position           workflowV2Position `json:"position"`
}

type workflowV2Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type workflowV2Edge struct {
	ID       string `json:"id"`
	SourceID string `json:"sourceId"`
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
}

type workflowV2NodeRun struct {
	WorkflowTaskRunID uuid.UUID  `json:"workflowTaskRunId"`
	RevisionID        uuid.UUID  `json:"revisionId"`
	NodeID            string     `json:"nodeId"`
	ChildTaskRunID    *uuid.UUID `json:"childTaskRunId,omitempty"`
	Status            string     `json:"status"`
	Attempt           uint32     `json:"attempt"`
}

type workflowV2RunSnapshot struct {
	Task       taskV2Record        `json:"task"`
	TaskRun    taskV2Run           `json:"taskRun"`
	Revision   workflowV2Revision  `json:"revision"`
	NodeRuns   []workflowV2NodeRun `json:"nodeRuns"`
	ChildTasks []taskV2Record      `json:"childTasks"`
}

func decodeWorkflowV2Revision(value any) (workflowV2Revision, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return workflowV2Revision{}, errRPCInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var revision workflowV2Revision
	if err := decoder.Decode(&revision); err != nil {
		return workflowV2Revision{}, errRPCInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return workflowV2Revision{}, errRPCInvalid
	}
	return revision, nil
}

func normalizeWorkflowV2Revision(
	project registeredProject,
	parent taskV2Definition,
	revision workflowV2Revision,
	expectedVersion uint64,
	now time.Time,
) (workflowV2Revision, error) {
	if parent.ID == uuid.Nil || parent.ProjectID != project.ID || parent.Kind != "workflow" || parent.Scope != "topLevel" ||
		revision.ID == uuid.Nil || revision.WorkflowTaskID != parent.ID || revision.Version != expectedVersion || expectedVersion == 0 || now.IsZero() ||
		len(revision.Nodes) < 3 || len(revision.Nodes) > maximumWorkflowV2Nodes || len(revision.Edges) < 2 || len(revision.Edges) > maximumWorkflowV2Edges ||
		len([]byte(revision.Description)) > maximumWorkflowV2Description || strings.IndexByte(revision.Description, 0) >= 0 ||
		revision.MaximumParallelism > maximumWorkflowV2Parallelism {
		return workflowV2Revision{}, errRPCInvalid
	}
	revision.Description = strings.TrimSpace(revision.Description)
	if revision.FailurePolicy == "" {
		revision.FailurePolicy = "stopOnFailure"
	}
	if !slices.Contains(workflowV2FailurePolicies, revision.FailurePolicy) {
		return workflowV2Revision{}, errRPCInvalid
	}

	nodeIDs := make(map[string]workflowV2Node, len(revision.Nodes))
	taskDefinitionIDs := make(map[uuid.UUID]struct{})
	startCount, finishCount, taskCount := 0, 0, 0
	for index := range revision.Nodes {
		node := &revision.Nodes[index]
		node.ID = strings.TrimSpace(node.ID)
		if !validWorkflowV2Identifier(node.ID) || !slices.Contains(workflowV2NodeTypes, node.Type) ||
			math.IsNaN(node.Position.X) || math.IsNaN(node.Position.Y) || math.IsInf(node.Position.X, 0) || math.IsInf(node.Position.Y, 0) ||
			math.Abs(node.Position.X) > 1e6 || math.Abs(node.Position.Y) > 1e6 {
			return workflowV2Revision{}, errRPCInvalid
		}
		if _, duplicate := nodeIDs[node.ID]; duplicate {
			return workflowV2Revision{}, errRPCInvalid
		}
		switch node.Type {
		case "start":
			startCount++
		case "finish":
			finishCount++
		case "task":
			taskCount++
		}
		if node.Type != "task" {
			if node.TaskDefinitionID != nil || node.TaskDefinition != nil || node.SourceTaskID != nil || node.SourceTaskRevision != nil {
				return workflowV2Revision{}, errRPCInvalid
			}
			nodeIDs[node.ID] = *node
			continue
		}
		if node.TaskDefinition == nil {
			return workflowV2Revision{}, errRPCInvalid
		}
		definition := *node.TaskDefinition
		if definition.ID == uuid.Nil || definition.Kind == "workflow" || definition.ProjectID != uuid.Nil && definition.ProjectID != project.ID ||
			definition.Scope != "" && definition.Scope != "workflowNode" ||
			definition.OwnerWorkflowTaskID != nil && *definition.OwnerWorkflowTaskID != parent.ID || definition.ParentTaskID != nil || definition.RootTaskID != nil ||
			definition.Execution.ScheduledAt != nil || definition.Execution.RunImmediately || definition.Execution.ResumeCliSession ||
			definition.Execution.CliSessionID != "" || len(definition.Execution.RelatedTaskIDs) != 0 {
			return workflowV2Revision{}, errRPCInvalid
		}
		if node.TaskDefinitionID != nil && *node.TaskDefinitionID != definition.ID {
			return workflowV2Revision{}, errRPCInvalid
		}
		if _, duplicate := taskDefinitionIDs[definition.ID]; duplicate {
			return workflowV2Revision{}, errRPCInvalid
		}
		taskDefinitionIDs[definition.ID] = struct{}{}
		definition.ProjectID, definition.Scope = project.ID, "workflowNode"
		ownerID := parent.ID
		definition.OwnerWorkflowTaskID = &ownerID
		if strings.TrimSpace(definition.CWD) == "" {
			definition.CWD = parent.CWD
		}
		definition.Execution = taskV2ExecutionOptions{
			Relation: "dependency", Mode: "parallel", WorkflowID: &ownerID,
		}
		normalized, err := normalizeTaskV2Definition(project, definition)
		if err != nil {
			return workflowV2Revision{}, err
		}
		definitionID := normalized.ID
		node.TaskDefinitionID, node.TaskDefinition = &definitionID, &normalized
		if node.SourceTaskID == nil && node.SourceTaskRevision != nil || node.SourceTaskID != nil && (node.SourceTaskRevision == nil || *node.SourceTaskRevision == 0) {
			return workflowV2Revision{}, errRPCInvalid
		}
		nodeIDs[node.ID] = *node
	}
	if startCount != 1 || finishCount != 1 || taskCount == 0 {
		return workflowV2Revision{}, errRPCInvalid
	}

	edgeIDs := make(map[string]struct{}, len(revision.Edges))
	edgeKeys := make(map[string]struct{}, len(revision.Edges))
	adjacency := make(map[string][]string, len(revision.Nodes))
	reverse := make(map[string][]string, len(revision.Nodes))
	for index := range revision.Edges {
		edge := &revision.Edges[index]
		edge.ID, edge.SourceID, edge.TargetID, edge.Label = strings.TrimSpace(edge.ID), strings.TrimSpace(edge.SourceID), strings.TrimSpace(edge.TargetID), strings.TrimSpace(edge.Label)
		if edge.Type == "" {
			edge.Type = "onSuccess"
		}
		source, sourceFound := nodeIDs[edge.SourceID]
		target, targetFound := nodeIDs[edge.TargetID]
		if !validWorkflowV2Identifier(edge.ID) || !sourceFound || !targetFound || edge.SourceID == edge.TargetID ||
			!slices.Contains(workflowV2EdgeTypes, edge.Type) || target.Type == "start" || source.Type == "finish" ||
			len([]byte(edge.Label)) > 200 || strings.IndexByte(edge.Label, 0) >= 0 {
			return workflowV2Revision{}, errRPCInvalid
		}
		if source.Type == "start" && edge.Type == "onFailure" {
			return workflowV2Revision{}, errRPCInvalid
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			return workflowV2Revision{}, errRPCInvalid
		}
		edgeIDs[edge.ID] = struct{}{}
		key := edge.SourceID + "\x00" + edge.TargetID + "\x00" + edge.Type
		if _, duplicate := edgeKeys[key]; duplicate {
			return workflowV2Revision{}, errRPCInvalid
		}
		edgeKeys[key] = struct{}{}
		adjacency[edge.SourceID] = append(adjacency[edge.SourceID], edge.TargetID)
		reverse[edge.TargetID] = append(reverse[edge.TargetID], edge.SourceID)
	}
	startID, finishID := "", ""
	for _, node := range revision.Nodes {
		if node.Type == "start" {
			startID = node.ID
		} else if node.Type == "finish" {
			finishID = node.ID
		}
	}
	if len(workflowV2Reachable(startID, adjacency)) != len(revision.Nodes) || len(workflowV2Reachable(finishID, reverse)) != len(revision.Nodes) {
		return workflowV2Revision{}, errRPCInvalid
	}
	if _, ok := workflowV2TopologicalOrder(revision.Nodes, revision.Edges); !ok {
		return workflowV2Revision{}, errRPCInvalid
	}

	sort.Slice(revision.Nodes, func(left, right int) bool { return revision.Nodes[left].ID < revision.Nodes[right].ID })
	sort.Slice(revision.Edges, func(left, right int) bool { return revision.Edges[left].ID < revision.Edges[right].ID })
	providedDigest := strings.TrimSpace(revision.GraphDigest)
	revision.GraphDigest = ""
	revision.CreatedAt = time.Time{}
	digest, err := workflowV2GraphDigest(revision)
	if err != nil || providedDigest != "" && providedDigest != digest {
		return workflowV2Revision{}, errRPCInvalid
	}
	revision.GraphDigest, revision.CreatedAt = digest, now.UTC().Truncate(time.Millisecond)
	return revision, nil
}

func workflowV2GraphDigest(revision workflowV2Revision) (string, error) {
	semantic := revision
	semantic.GraphDigest = ""
	semantic.CreatedAt = time.Time{}
	encoded, err := json.Marshal(semantic)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return "", errRPCInvalid
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func bindWorkflowV2Definition(project registeredProject, definition taskV2Definition, revision workflowV2Revision) (taskV2Definition, error) {
	if definition.ID != revision.WorkflowTaskID || definition.Kind != "workflow" || definition.Scope != "topLevel" || definition.ProjectID != project.ID {
		return taskV2Definition{}, errRPCInvalid
	}
	config, err := json.Marshal(map[string]any{"revisionId": revision.ID, "revisionVersion": revision.Version})
	if err != nil {
		return taskV2Definition{}, errRPCInvalid
	}
	definition.Config = config
	return normalizeTaskV2Definition(project, definition)
}

func workflowV2DefinitionRevision(config json.RawMessage) (uuid.UUID, uint64, error) {
	decoded, err := decodeTaskRunnerConfig(config)
	if err != nil || !taskMapHasOnly(decoded, "revisionId", "revisionVersion") {
		return uuid.Nil, 0, errRPCInvalid
	}
	rawID, ok := decoded["revisionId"].(string)
	revisionID, idErr := uuid.Parse(strings.TrimSpace(rawID))
	version, versionOK := taskJSONUint64(decoded["revisionVersion"])
	if !ok || idErr != nil || revisionID == uuid.Nil || !versionOK || version == 0 {
		return uuid.Nil, 0, errRPCInvalid
	}
	return revisionID, version, nil
}

func taskJSONUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return uint64(parsed), err == nil && parsed > 0
	case float64:
		return uint64(typed), typed >= 1 && typed <= float64(^uint64(0)) && math.Trunc(typed) == typed
	default:
		return 0, false
	}
}

func validWorkflowV2Identifier(value string) bool {
	if value == "" || len([]byte(value)) > maximumWorkflowV2Identifier || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func workflowV2Reachable(start string, adjacency map[string][]string) map[string]struct{} {
	visited := map[string]struct{}{}
	stack := []string{start}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if _, found := visited[current]; found {
			continue
		}
		visited[current] = struct{}{}
		stack = append(stack, adjacency[current]...)
	}
	return visited
}

func workflowV2TopologicalOrder(nodes []workflowV2Node, edges []workflowV2Edge) ([]string, bool) {
	indegree := make(map[string]int, len(nodes))
	adjacency := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		indegree[node.ID] = 0
	}
	for _, edge := range edges {
		indegree[edge.TargetID]++
		adjacency[edge.SourceID] = append(adjacency[edge.SourceID], edge.TargetID)
	}
	ready := make([]string, 0)
	for id, count := range indegree {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	result := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		result = append(result, current)
		for _, target := range adjacency[current] {
			indegree[target]--
			if indegree[target] == 0 {
				ready = append(ready, target)
				sort.Strings(ready)
			}
		}
	}
	return result, len(result) == len(nodes)
}

func validWorkflowV2NodeStatus(value string) bool {
	return slices.Contains(workflowV2NodeStatuses, value)
}
