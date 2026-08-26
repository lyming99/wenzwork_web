package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAIQuestionsPerCall     = 8
	maximumAIQuestionBytes        = 4096
	maximumAIQuestionHeaderBytes  = 200
	maximumAIQuestionOptions      = 8
	maximumAIQuestionOptionBytes  = 500
	maximumAIQuestionAnswerBytes  = 4096
	maximumAIQuestionIDBytes      = 64
)

// aiQuestionOption is one selectable choice rendered for the user. The first
// option is conventionally the recommended one.
type aiQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// aiQuestion is one structured question the model asks mid-turn. The stable
// ID is echoed back in the user's answer.
type aiQuestion struct {
	ID          string            `json:"id"`
	Question    string            `json:"question"`
	Header      string            `json:"header,omitempty"`
	Options     []aiQuestionOption `json:"options,omitempty"`
	MultiSelect bool              `json:"multiSelect,omitempty"`
}

// aiQuestionAnswer pairs a stable question id with the user's free-form or
// option answer.
type aiQuestionAnswer struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

type aiQuestionResolution struct {
	Decision string // answered | timeout | cancelled
	Answers  []aiQuestionAnswer
}

type pendingAIQuestion struct {
	ProjectID      uuid.UUID
	ConversationID string
	GenerationID   string
	ToolCallID     string
	QuestionIDs    []string
	ExpiresAt      time.Time
	decision       chan aiQuestionResolution
}

func parseAIQuestions(raw any) ([]aiQuestion, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, errRPCInvalid
	}
	var questions []aiQuestion
	if err := json.Unmarshal(encoded, &questions); err != nil {
		return nil, errRPCInvalid
	}
	if len(questions) == 0 || len(questions) > maximumAIQuestionsPerCall {
		return nil, errRPCInvalid
	}
	seen := make(map[string]struct{}, len(questions))
	for _, question := range questions {
		if !validAIQuestion(question) {
			return nil, errRPCInvalid
		}
		if _, duplicate := seen[question.ID]; duplicate {
			return nil, errRPCInvalid
		}
		seen[question.ID] = struct{}{}
	}
	return questions, nil
}

func validAIQuestion(question aiQuestion) bool {
	if len(question.ID) == 0 || len(question.ID) > maximumAIQuestionIDBytes ||
		strings.TrimSpace(question.Question) == "" || len(question.Question) > maximumAIQuestionBytes ||
		!utf8.ValidString(question.ID) || !utf8.ValidString(question.Question) ||
		len(question.Header) > maximumAIQuestionHeaderBytes || !utf8.ValidString(question.Header) ||
		len(question.Options) > maximumAIQuestionOptions {
		return false
	}
	for _, option := range question.Options {
		if strings.TrimSpace(option.Label) == "" || len(option.Label) > maximumAIQuestionOptionBytes ||
			len(option.Description) > maximumAIQuestionOptionBytes ||
			!utf8.ValidString(option.Label) || !utf8.ValidString(option.Description) {
			return false
		}
	}
	return true
}

func validAIQuestionAnswer(answer aiQuestionAnswer) bool {
	return answer.ID != "" && len(answer.ID) <= maximumAIQuestionIDBytes && utf8.ValidString(answer.ID) &&
		utf8.ValidString(answer.Answer) && len(answer.Answer) <= maximumAIQuestionAnswerBytes
}

func (state *agentState) registerAIQuestion(projectID uuid.UUID, conversationID, generationID, toolCallID string, questionIDs []string, expiresAt time.Time) (*pendingAIQuestion, error) {
	if state == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		toolCallID == "" || len(toolCallID) > 256 || len(questionIDs) == 0 || len(questionIDs) > maximumAIQuestionsPerCall || expiresAt.IsZero() {
		return nil, errRPCInvalid
	}
	pending := &pendingAIQuestion{
		ProjectID: projectID, ConversationID: conversationID, GenerationID: generationID,
		ToolCallID: toolCallID, QuestionIDs: append([]string(nil), questionIDs...), ExpiresAt: expiresAt,
		decision: make(chan aiQuestionResolution, 1),
	}
	state.aiQuestionMu.Lock()
	defer state.aiQuestionMu.Unlock()
	if state.aiQuestions == nil {
		state.aiQuestions = make(map[string]*pendingAIQuestion)
	}
	for _, existing := range state.aiQuestions {
		if existing.GenerationID == generationID && existing.ToolCallID == toolCallID {
			return nil, errRPCRevision
		}
	}
	state.aiQuestions[toolCallID] = pending
	return pending, nil
}

func (state *agentState) waitAIQuestion(ctx context.Context, pending *pendingAIQuestion) aiQuestionResolution {
	if state == nil || pending == nil {
		return aiQuestionResolution{Decision: "cancelled"}
	}
	duration := time.Until(pending.ExpiresAt)
	if duration < 0 {
		duration = 0
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case resolution := <-pending.decision:
		return resolution
	case <-timer.C:
		state.removeAIQuestion(pending.ToolCallID, pending)
		return aiQuestionResolution{Decision: "timeout"}
	case <-ctx.Done():
		state.removeAIQuestion(pending.ToolCallID, pending)
		return aiQuestionResolution{Decision: "cancelled"}
	}
}

// resolveAIQuestion validates the answer set against the pending question and
// delivers it to the blocked tool call. Unknown or unmatched answers are
// rejected wholesale so the model never sees partial answers.
func (state *agentState) resolveAIQuestion(projectID uuid.UUID, conversationID, generationID, toolCallID string, answers []aiQuestionAnswer) error {
	if state == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		toolCallID == "" || len(toolCallID) > 256 || len(answers) == 0 || len(answers) > maximumAIQuestionsPerCall {
		return errRPCInvalid
	}
	for _, answer := range answers {
		if !validAIQuestionAnswer(answer) {
			return errRPCInvalid
		}
	}
	state.aiQuestionMu.Lock()
	pending := state.aiQuestions[toolCallID]
	if pending == nil {
		state.aiQuestionMu.Unlock()
		return errRPCNotFound
	}
	if pending.ProjectID != projectID || pending.ConversationID != conversationID || pending.GenerationID != generationID {
		state.aiQuestionMu.Unlock()
		return errRPCRevision
	}
	answered := make(map[string]struct{}, len(answers))
	for _, answer := range answers {
		if _, duplicate := answered[answer.ID]; duplicate {
			state.aiQuestionMu.Unlock()
			return errRPCInvalid
		}
		answered[answer.ID] = struct{}{}
	}
	for _, questionID := range pending.QuestionIDs {
		if _, present := answered[questionID]; !present {
			state.aiQuestionMu.Unlock()
			return errRPCInvalid
		}
	}
	delete(state.aiQuestions, toolCallID)
	state.aiQuestionMu.Unlock()
	pending.decision <- aiQuestionResolution{Decision: "answered", Answers: append([]aiQuestionAnswer(nil), answers...)}
	return nil
}

func (state *agentState) removeAIQuestion(toolCallID string, expected *pendingAIQuestion) {
	if state == nil {
		return
	}
	state.aiQuestionMu.Lock()
	if state.aiQuestions[toolCallID] == expected {
		delete(state.aiQuestions, toolCallID)
	}
	state.aiQuestionMu.Unlock()
}

func (state *agentState) clearAIGenerationQuestions(generationID string) {
	if state == nil {
		return
	}
	state.aiQuestionMu.Lock()
	for toolCallID, pending := range state.aiQuestions {
		if pending.GenerationID == generationID {
			delete(state.aiQuestions, toolCallID)
			select {
			case pending.decision <- aiQuestionResolution{Decision: "cancelled"}:
			default:
			}
		}
	}
	state.aiQuestionMu.Unlock()
}

// aiQuestionToolValue is the model-facing result value: the asked questions
// plus the user's answers keyed by stable id.
func aiQuestionToolValue(questions []aiQuestion, answers []aiQuestionAnswer) map[string]any {
	answerMap := make(map[string]string, len(answers))
	for _, answer := range answers {
		answerMap[answer.ID] = answer.Answer
	}
	value := map[string]any{"questions": questions, "answers": answers, "answered": answerMap}
	return value
}
