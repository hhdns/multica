package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SignalType constants mirror the agent_interaction_signal.type column.
const (
	SignalTypePraise    = "praise"
	SignalTypeCriticism = "criticism"
	SignalTypeSuccess   = "task_success"
	SignalTypeFailure   = "task_failure"
)

// driftBatchThreshold is how many unprocessed signals must accumulate
// before a trait-drift pass runs. Keeps the drift from being too reactive.
const driftBatchThreshold = 5

// classifyMaxTok is the token budget for LLM-based comment classification.
// Tight because we only need a small JSON object back.
const classifyMaxTok = 80

// ClassifyCommentSignal detects praise or criticism in a human comment.
// If a synthesis backend is configured (Anthropic or OpenAI-compat), it uses
// an LLM for accurate classification. Otherwise it falls back to keyword matching.
func ClassifyCommentSignal(ctx context.Context, content string) (signalType string, weight float32, ok bool) {
	cfg := resolveSynthesisConfig()
	if cfg.backend != "" {
		st, w, err := classifyCommentWithLLM(ctx, cfg, content)
		if err != nil {
			slog.Warn("persona: LLM comment classification failed, using keywords", "error", err)
		} else {
			if st == "neutral" || st == "" {
				return "", 0, false
			}
			return st, w, true
		}
	}
	return detectCommentKeywords(content)
}

// classifyCommentWithLLM asks the configured LLM backend to classify a comment.
// Returns signalType ("praise"/"criticism"/"neutral") and weight (0.1–1.0).
func classifyCommentWithLLM(ctx context.Context, cfg synthesisConfig, content string) (signalType string, weight float32, err error) {
	prompt := fmt.Sprintf(
		`Classify this comment about an AI agent's work. Output ONLY JSON with no explanation: {"type":"praise"|"criticism"|"neutral","weight":0.1-1.0}
Weight: 1.0=very strong, 0.5=moderate, 0.1=subtle.

Comment: %q`, content)

	var raw string
	switch cfg.backend {
	case "anthropic":
		raw, err = callAnthropic(ctx, cfg, prompt, classifyMaxTok)
	case "openai":
		raw, err = callOpenAICompat(ctx, cfg, prompt, classifyMaxTok)
	default:
		return "", 0, fmt.Errorf("unknown backend: %s", cfg.backend)
	}
	if err != nil {
		return "", 0, err
	}

	// Strip markdown fences or leading text if the model wraps its output.
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if i := strings.LastIndex(raw, "}"); i >= 0 && i < len(raw)-1 {
		raw = raw[:i+1]
	}

	var result struct {
		Type   string  `json:"type"`
		Weight float32 `json:"weight"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", 0, fmt.Errorf("parse classification response %q: %w", raw, err)
	}
	if result.Weight < 0.1 || result.Weight > 1.0 {
		result.Weight = 0.5
	}
	return result.Type, result.Weight, nil
}

// detectCommentKeywords is the keyword-only fallback for comment classification.
// Used when no LLM backend is configured or when the LLM call fails.
func detectCommentKeywords(content string) (signalType string, weight float32, ok bool) {
	return DetectCommentSignal(content)
}

// DetectCommentSignal inspects comment text for explicit praise or
// criticism directed at an agent. Returns the signal type, a weight
// (0.1–1.0), and whether a signal was found at all.
//
// Keyword-only — use ClassifyCommentSignal for LLM-backed detection.
// Weights: 0.6 for praise, 0.7 for criticism (bad feedback lands harder).
func DetectCommentSignal(content string) (signalType string, weight float32, ok bool) {
	lower := strings.ToLower(content)

	praiseKeywords := []string{
		"great job", "great work", "well done", "nicely done", "good job",
		"perfect", "excellent", "exactly right", "love it", "love this",
		"this is great", "this is perfect", "this is excellent",
		"thank you", "thanks", "helpful", "awesome", "brilliant",
		"very good", "looks good", "lgtm",
		"很好", "做得好", "不错", "完美", "棒", "赞", "厉害", "很棒", "太好了",
		"感谢", "谢谢", "正确", "对了", "就是这样",
	}

	criticismKeywords := []string{
		"wrong", "incorrect", "not right", "that's wrong", "this is wrong",
		"fix this", "this is bad", "you missed", "you forgot",
		"this doesn't work", "this is broken", "not what i asked",
		"not what i wanted", "please redo", "try again",
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

// RecordCommentSignal creates an agent_interaction_signal row, bumps the
// persona signal count, and — if enough signals have accumulated — runs a
// trait-drift pass. Always called in a goroutine; errors are logged only.
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
		AgentID:      agentID,
		WorkspaceID:  workspaceID,
		Type:         signalType,
		Content:      truncateSignalContent(content),
		Weight:       weight,
		SourceType:   "comment",
		SourceID:     commentID,
		SourceUserID: userID,
	})
	if err != nil {
		slog.Warn("persona: failed to record comment signal", "error", err,
			"agent_id", agentID, "type", signalType)
		return
	}

	// Strong praise or criticism leaves an emotional trace in episodic memory.
	if signalType == SignalTypePraise || signalType == SignalTypeCriticism {
		MaybeRecordEmotionalImpression(ctx, q, agentID, workspaceID, signalType, weight, content)
	}

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

	MaybeApplyTraitDrift(ctx, q, agentID)
}

// MaybeApplyTraitDrift runs a trait-drift pass if enough unprocessed signals
// have built up. Safe to call from a goroutine; all errors are logged.
func MaybeApplyTraitDrift(ctx context.Context, q *db.Queries, agentID pgtype.UUID) {
	signals, err := q.ListUnprocessedAgentSignals(ctx, agentID)
	if err != nil || len(signals) < driftBatchThreshold {
		return
	}

	// Compute deltas from the batch. Each signal contributes according to its
	// type and weight. Deltas are clamped in the SQL (GREATEST/LEAST 0-100).
	var (
		dThoroughness  float32
		dVerbosity     float32
		dRiskAppetite  float32
		dCuriosity     float32
		dConfidence    float32
	)

	for _, s := range signals {
		w := s.Weight
		switch s.Type {
		case SignalTypePraise:
			// Praise boosts confidence and thoroughness (they're doing it right).
			dConfidence += w * 2.5
			dThoroughness += w * 1.0
		case SignalTypeCriticism:
			// Criticism dents confidence and makes the agent more conservative.
			dConfidence -= w * 2.0
			dRiskAppetite -= w * 1.5
			// Tiny curiosity bump — criticism often comes with new information.
			dCuriosity += w * 0.5
		case SignalTypeSuccess:
			dConfidence += w * 1.5
		case SignalTypeFailure:
			dConfidence -= w * 1.0
			dRiskAppetite -= w * 1.0
		}
	}

	// Dampen: cap any single batch at ±8 points per trait so one bad day
	// doesn't reshape the agent's whole character.
	clampDelta := func(v float32) int32 {
		if v > 8 {
			v = 8
		} else if v < -8 {
			v = -8
		}
		return int32(v)
	}

	_, err = q.DriftAgentPersonaTraits(ctx, db.DriftAgentPersonaTraitsParams{
		AgentID:           agentID,
		TraitThoroughness: clampDelta(dThoroughness),
		TraitVerbosity:    clampDelta(dVerbosity),
		TraitRiskAppetite: clampDelta(dRiskAppetite),
		TraitCuriosity:    clampDelta(dCuriosity),
		TraitConfidence:   clampDelta(dConfidence),
	})
	if err != nil {
		slog.Warn("persona: trait drift failed", "error", err, "agent_id", agentID)
		return
	}

	if err := q.MarkAgentSignalsProcessed(ctx, agentID); err != nil {
		slog.Warn("persona: mark signals processed failed", "error", err, "agent_id", agentID)
	}

	// Auto-synthesize instructions every synthesisAfterBatches drift batches.
	MaybeSynthesizeAfterDrift(ctx, q, agentID)
}

// UpdateAgentMood recalculates and persists the agent's mood based on its
// recent 7-day task success rate and its variance_level (spontaneity).
// Safe to call from a goroutine after CompleteTask / FailTask.
// No-ops gracefully when no persona row exists yet for this agent.
func UpdateAgentMood(ctx context.Context, q *db.Queries, agentID pgtype.UUID) {
	persona, err := q.GetAgentPersona(ctx, agentID)
	if err != nil {
		// No persona row yet — nothing to update.
		return
	}

	outcomes, err := q.CountRecentAgentTaskOutcomes(ctx, agentID)
	if err != nil {
		slog.Warn("persona: count task outcomes failed", "error", err, "agent_id", agentID)
		return
	}

	total := outcomes.CompletedCount + outcomes.FailedCount
	var mood string
	switch {
	case total == 0:
		mood = "calm"
	default:
		successRate := float64(outcomes.CompletedCount) / float64(total)
		switch {
		case successRate >= 0.8:
			mood = "energized"
		case successRate <= 0.35:
			mood = "cautious"
		default:
			mood = "calm"
		}
	}

	// Spontaneity: high variance_level gives a small chance of "playful"
	// even when things are calm — the "random spark" effect.
	if mood == "calm" && persona.VarianceLevel > 55 {
		if rand.Float32() < float32(persona.VarianceLevel-55)/150.0 {
			mood = "playful"
		}
	}

	if mood == persona.Mood {
		return // nothing changed, skip the write
	}

	if _, err := q.UpdateAgentPersonaMood(ctx, db.UpdateAgentPersonaMoodParams{
		AgentID: agentID,
		Mood:    mood,
	}); err != nil {
		slog.Warn("persona: mood update failed", "error", err, "agent_id", agentID)
	}
}

func truncateSignalContent(s string) string {
	runes := []rune(s)
	if len(runes) > 500 {
		return string(runes[:500]) + "…"
	}
	return s
}
