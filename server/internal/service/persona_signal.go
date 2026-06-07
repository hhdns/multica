package service

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SignalType constants mirror the agent_interaction_signal.type column.
const (
	SignalTypePraise    = "praise"
	SignalTypeCriticism = "criticism"
)

// DetectCommentSignal inspects comment text for explicit praise or
// criticism directed at an agent. Returns the signal type, a weight
// (0.1–1.0), and whether a signal was found at all.
//
// This is intentionally keyword-based for the MVP — no LLM call on the
// hot comment-creation path. Weights are coarse: 0.6 for praise, 0.7
// for criticism (criticism leaves a stronger impression, like humans).
func DetectCommentSignal(content string) (signalType string, weight float32, ok bool) {
	lower := strings.ToLower(content)

	praiseKeywords := []string{
		"great job", "great work", "well done", "nicely done", "good job",
		"perfect", "excellent", "exactly right", "love it", "love this",
		"this is great", "this is perfect", "this is excellent",
		"thank you", "thanks", "helpful", "awesome", "brilliant",
		"very good", "looks good", "lgtm",
		// Chinese
		"很好", "做得好", "不错", "完美", "棒", "赞", "厉害", "很棒", "太好了",
		"感谢", "谢谢", "正确", "对了", "就是这样",
	}

	criticismKeywords := []string{
		"wrong", "incorrect", "not right", "that's wrong", "this is wrong",
		"fix this", "this is bad", "you missed", "you forgot",
		"this doesn't work", "this is broken", "not what i asked",
		"not what i wanted", "please redo", "try again",
		// Chinese
		"不对", "错了", "不行", "有问题", "不好", "改一下", "重做",
		"这不对", "你搞错了", "重新来", "不是我要的",
	}

	for _, kw := range praiseKeywords {
		if strings.Contains(lower, kw) {
			return SignalTypePraise, 0.6, true
		}
	}
	for _, kw := range criticismKeywords {
		if strings.Contains(lower, kw) {
			return SignalTypeCriticism, 0.7, true
		}
	}
	return "", 0, false
}

// RecordCommentSignal creates an agent_interaction_signal row and bumps
// the persona signal count. It is always called in a goroutine — errors
// are logged, not propagated.
func RecordCommentSignal(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID pgtype.UUID,
	signalType string,
	weight float32,
	content string,
	commentID pgtype.UUID,
	userID pgtype.UUID,
) {
	_, err := q.CreateAgentInteractionSignal(ctx, db.CreateAgentInteractionSignalParams{
		AgentID:       agentID,
		WorkspaceID:   workspaceID,
		Type:          signalType,
		Content:       truncateSignalContent(content),
		Weight:        weight,
		SourceType:    "comment",
		SourceID:      commentID,
		SourceUserID:  userID,
	})
	if err != nil {
		slog.Warn("persona: failed to record comment signal", "error", err,
			"agent_id", agentID, "type", signalType)
		return
	}

	// Ensure a persona row exists, then bump its counter.
	if _, err := q.UpsertAgentPersona(ctx, db.UpsertAgentPersonaParams{
		AgentID:     agentID,
		WorkspaceID: workspaceID,
	}); err != nil {
		slog.Warn("persona: failed to upsert persona on signal", "error", err, "agent_id", agentID)
		return
	}
	if err := q.IncrementAgentPersonaSignalCount(ctx, agentID); err != nil {
		slog.Warn("persona: failed to increment signal count", "error", err, "agent_id", agentID)
	}
}

func truncateSignalContent(s string) string {
	runes := []rune(s)
	if len(runes) > 500 {
		return string(runes[:500]) + "…"
	}
	return s
}
