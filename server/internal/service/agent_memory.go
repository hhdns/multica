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
