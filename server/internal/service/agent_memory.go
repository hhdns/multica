package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// Maximum number of memories retained per agent. Pruning uses a
	// multi-factor retention score, not pure age — see DeleteOldAgentMemories.
	memoryRetentionLimit = 200
	// Number of memories retrieved for a task context injection.
	memorySearchTopK = 5
	// Minimum cosine similarity threshold to include a memory in the brief.
	memoryMinSimilarity = 0.70
)

// embedConfig is resolved from environment variables, mirroring the pattern
// used by resolveSynthesisConfig so operators configure both with the same
// set of env vars.
//
// Resolution order:
//  1. PERSONA_SYNTHESIS_BASE_URL set → OpenAI-compat embedding endpoint
//     Uses POST <base_url>/embeddings with the OpenAI request shape.
//  2. OPENAI_API_KEY set (and no base URL) → OpenAI direct
//  3. ANTHROPIC_API_KEY set → not used for embeddings (Anthropic has no
//     embedding API); falls through to disabled.
//  4. Neither → embedding disabled; memories are stored without vectors.
type embedConfig struct {
	endpoint string
	apiKey   string
	model    string
}

func resolveEmbedConfig() embedConfig {
	if strings.EqualFold(os.Getenv("PERSONA_SYNTHESIS_ENABLED"), "false") {
		return embedConfig{}
	}

	// OpenAI-compat endpoint (covers Ollama nomic-embed-text, local vLLM, etc.)
	if base := os.Getenv("PERSONA_SYNTHESIS_BASE_URL"); base != "" {
		model := os.Getenv("PERSONA_EMBEDDING_MODEL")
		if model == "" {
			model = "nomic-embed-text" // sensible default for Ollama
		}
		return embedConfig{
			endpoint: strings.TrimRight(base, "/") + "/embeddings",
			apiKey:   os.Getenv("PERSONA_SYNTHESIS_API_KEY"),
			model:    model,
		}
	}

	// OpenAI direct
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		model := os.Getenv("PERSONA_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
		return embedConfig{
			endpoint: "https://api.openai.com/v1/embeddings",
			apiKey:   key,
			model:    model,
		}
	}

	return embedConfig{}
}

// Embed calls the configured embedding API and returns a float32 vector.
// Returns nil when embedding is disabled or fails — callers store the memory
// without a vector in that case.
func Embed(ctx context.Context, text string) []float32 {
	cfg := resolveEmbedConfig()
	if cfg.endpoint == "" {
		return nil
	}

	type embedRequest struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	type embedData struct {
		Embedding []float32 `json:"embedding"`
	}
	type embedResponse struct {
		Data  []embedData `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	body, err := json.Marshal(embedRequest{Model: cfg.model, Input: text})
	if err != nil {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	}

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		slog.Debug("embed: HTTP error", "error", err)
		return nil
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil || er.Error != nil || len(er.Data) == 0 {
		if er.Error != nil {
			slog.Debug("embed: API error", "message", er.Error.Message)
		}
		return nil
	}
	return er.Data[0].Embedding
}

// RecordTaskMemory creates a memory entry after a task completes or fails.
// Called as a goroutine from task completion handlers; errors are logged only.
//
// content should be a plain-text summary of what the agent did / what
// happened — typically 1-3 sentences. The embedding is generated async:
// the row is inserted first, then updated with the vector.
func RecordTaskMemory(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
	workspaceID pgtype.UUID,
	issueID pgtype.UUID,
	taskID pgtype.UUID,
	content string,
	sentiment string, // "positive", "negative", "neutral"
	importance float32,
	emotionalValence float32,
	emotionalIntensity float32,
) {
	mem, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            content,
		Category:           "task_outcome",
		Sentiment:          sentiment,
		SourceIssueID:      issueID,
		SourceTaskID:       taskID,
		Importance:         importance,
		EmotionalValence:   emotionalValence,
		EmotionalIntensity: emotionalIntensity,
	})
	if err != nil {
		slog.Warn("agent_memory: create failed", "error", err, "agent_id", agentID)
		return
	}

	// Embed and update in the same goroutine.
	vec := Embed(ctx, content)
	if vec != nil {
		if err := q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
			ID:        mem.ID,
			Embedding: pgvector.NewVector(vec),
		}); err != nil {
			slog.Warn("agent_memory: set embedding failed", "error", err, "memory_id", mem.ID)
		}
	}

	// Prune old memories to stay within the retention cap.
	if err := q.DeleteOldAgentMemories(ctx, db.DeleteOldAgentMemoriesParams{
		AgentID: agentID,
		Offset:  memoryRetentionLimit,
	}); err != nil {
		slog.Warn("agent_memory: prune failed", "error", err, "agent_id", agentID)
	}
}

// SearchRelevantMemories returns a formatted markdown block of relevant past
// memories for injection into the runtime brief. Returns "" when no memories
// are relevant (embedding disabled, low similarity, or no memories at all).
//
// queryText should be derived from the current task's issue title/description
// — the caller assembles this from the task data available at claim time.
func SearchRelevantMemories(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
	queryText string,
) string {
	if queryText == "" {
		return ""
	}

	vec := Embed(ctx, queryText)
	if vec == nil {
		// Embedding disabled — fall back to the most recent memories.
		return recentMemoriesFallback(ctx, q, agentID)
	}

	results, err := q.SearchAgentMemories(ctx, db.SearchAgentMemoriesParams{
		AgentID:   agentID,
		Embedding: pgvector.NewVector(vec),
		Limit:     int32(memorySearchTopK),
	})
	if err != nil || len(results) == 0 {
		return ""
	}

	var b strings.Builder
	var accessedIDs []pgtype.UUID
	for _, r := range results {
		if r.Similarity < memoryMinSimilarity {
			continue
		}
		if len(accessedIDs) == 0 {
			b.WriteString("## Relevant Past Experience\n\n")
			b.WriteString("These memories from your past work may be relevant to the current task:\n\n")
		}
		sentiment := ""
		switch r.Sentiment {
		case "positive":
			sentiment = " ✓"
		case "negative":
			sentiment = " ✗"
		}
		fmt.Fprintf(&b, "- %s%s\n", r.Content, sentiment)
		accessedIDs = append(accessedIDs, r.ID)
	}

	// Bump access counts for memories that surfaced — frequently-recalled
	// memories score higher on retention and are less likely to be pruned.
	if len(accessedIDs) > 0 {
		if err := q.BumpMemoryAccess(ctx, accessedIDs); err != nil {
			slog.Debug("agent_memory: bump access failed", "error", err)
		}
	}

	return b.String()
}

// recentMemoriesFallback returns the 3 most recent memories as plain text
// when vector search is unavailable. This ensures agents always have some
// memory context even without an embedding API configured.
func recentMemoriesFallback(ctx context.Context, q *db.Queries, agentID pgtype.UUID) string {
	memories, err := q.ListAgentMemories(ctx, db.ListAgentMemoriesParams{
		AgentID: agentID,
		Limit:   3,
	})
	if err != nil || len(memories) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Recent Experience\n\n")
	for _, m := range memories {
		fmt.Fprintf(&b, "- %s\n", m.Content)
	}
	b.WriteString("\n")
	return b.String()
}

// ScoreTaskMemory computes importance, emotional_valence, and emotional_intensity
// for a task outcome memory based on contextual signals available at record time.
//
// Base values:
//   - completed: importance 0.50, valence +0.40, intensity 0.30
//   - failed:    importance 0.65, valence -0.50, intensity 0.55
//
// Adjustments applied on top of base:
//   +0.15 importance / +0.15 intensity  — issue had ≥5 comments (complex task)
//   +0.05 importance                    — issue had 2-4 comments (moderate)
//   +0.10 importance / +0.20 valence    — completed and issue is already done
//                                          (human confirmed the work)
//   +0.20 importance / +0.15 intensity  — re-trigger on previously-failed issue
//                                          (this is a genuine learning moment)
//   -0.10 importance / -0.10 intensity  — pure autopilot run, no human comment
//                                          (routine, less memorable)
func ScoreTaskMemory(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
	issue db.Issue,
	task *db.AgentTaskQueue,
	outcomeType string,
) (importance, valence, intensity float32) {
	// Base values by outcome.
	if outcomeType == "failed" {
		importance, valence, intensity = 0.65, -0.50, 0.55
	} else {
		importance, valence, intensity = 0.50, 0.40, 0.30
	}

	// Issue complexity: comment count as a proxy.
	commentCount, err := q.CountComments(ctx, db.CountCommentsParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err == nil {
		switch {
		case commentCount >= 5:
			importance += 0.15
			intensity += 0.15
		case commentCount >= 2:
			importance += 0.05
		}
	}

	// Human confirmed: issue closed after successful completion.
	if outcomeType == "completed" && (issue.Status == "done" || issue.Status == "cancelled") {
		importance += 0.10
		valence += 0.20
		intensity += 0.10
	}

	// Re-trigger: check whether this issue has a recent failed memory.
	// A second attempt after failure is a genuine learning moment.
	if task != nil && (task.TriggerCommentID.Valid || task.ChatSessionID.Valid) {
		if past, err := q.ListMemoriesForIssue(ctx, db.ListMemoriesForIssueParams{
			AgentID:       agentID,
			SourceIssueID: issue.ID,
			Limit:         5,
		}); err == nil {
			for _, m := range past {
				if m.Sentiment == "negative" {
					importance += 0.20
					intensity += 0.15
					break
				}
			}
		}
	}

	// Pure autopilot run with no human trigger → less memorable.
	if task != nil && task.AutopilotRunID.Valid && !task.TriggerCommentID.Valid && !task.ChatSessionID.Valid {
		importance -= 0.10
		intensity -= 0.10
	}

	// Clamp to valid ranges.
	importance = clamp32(importance, 0.10, 1.0)
	valence = clamp32(valence, -1.0, 1.0)
	intensity = clamp32(intensity, 0.0, 1.0)
	return
}

func clamp32(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SummarizeTaskOutcome generates a short memory string from task completion
// data. Called by task completion / failure handlers before RecordTaskMemory.
// Returns a 1-3 sentence human-readable summary suitable for storage and
// embedding. Falls back to a generic string if the LLM call fails.
func SummarizeTaskOutcome(
	ctx context.Context,
	issueTitle string,
	outcomeType string, // "completed" | "failed"
	triggerType string, // "on_assign" | "comment" | "chat" | "autopilot"
) string {
	// Build a short summary without LLM — simple and cheap.
	// For Phase 5 MVP, this is deterministic. A future iteration can replace
	// it with a Haiku call to extract key decisions from the task log.
	action := "completed"
	if outcomeType == "failed" {
		action = "attempted but did not complete"
	}
	trigger := ""
	switch triggerType {
	case "comment":
		trigger = " (triggered by a comment)"
	case "chat":
		trigger = " (via chat)"
	case "autopilot":
		trigger = " (autopilot run)"
	}
	if issueTitle != "" {
		return fmt.Sprintf("I %s work on: %s%s.", action, issueTitle, trigger)
	}
	return fmt.Sprintf("I %s a task%s.", action, trigger)
}
