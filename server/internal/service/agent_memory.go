package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// Tiered retention caps per category group. The SQL query
	// (DeleteOldAgentMemories) encodes the same values — keep them in sync.
	memoryLimitEpisodic  = 150 // task_outcome, user_feedback, self_note
	memoryLimitSkill     = 30  // skill_learned (consolidated)
	memoryLimitEmotional = 20  // emotional_impression
	// Total hard cap used for the compaction trigger threshold.
	memoryRetentionLimit = memoryLimitEpisodic + memoryLimitSkill + memoryLimitEmotional // 200
	// Number of memories retrieved for a task context injection.
	memorySearchTopK = 5
	// Minimum cosine similarity threshold to include a memory in the brief.
	memoryMinSimilarity = 0.70

	// system_config key that stores the embedding model used to generate
	// existing vectors. Compared against the live env var on startup to
	// detect when a model change makes old embeddings stale.
	embeddingModelConfigKey = "embedding_model"

	// rebuildBatchSize is the number of memories re-embedded per iteration
	// during a rebuild so the embedding service isn't overwhelmed.
	rebuildBatchSize = 20
)

// embeddingModelStale is an in-memory flag set on startup (InitEmbeddingModelCheck)
// and cleared after a successful rebuild. Read by GetConfig without a DB query.
var embeddingModelStale atomic.Bool

// CurrentEmbeddingModel returns the embedding model name from env, defaulting
// to bge-m3 — the same default used by resolveEmbedConfig.
func CurrentEmbeddingModel() string {
	if m := os.Getenv("PERSONA_EMBEDDING_MODEL"); m != "" {
		return m
	}
	return "bge-m3"
}

// InitEmbeddingModelCheck compares the live embedding model against the value
// stored in system_config and sets the in-memory stale flag accordingly.
// Call once during server startup after the DB is reachable.
func InitEmbeddingModelCheck(ctx context.Context, q *db.Queries) {
	stored, err := q.GetSystemConfig(ctx, embeddingModelConfigKey)
	if err != nil {
		// No row yet — first run. Record the current model as the baseline.
		_ = q.UpsertSystemConfig(ctx, db.UpsertSystemConfigParams{
			Key:   embeddingModelConfigKey,
			Value: CurrentEmbeddingModel(),
		})
		return
	}
	embeddingModelStale.Store(stored != CurrentEmbeddingModel())
}

// IsEmbeddingModelStale returns true when the embedding model has changed
// since embeddings were last generated, meaning old vectors are incompatible.
func IsEmbeddingModelStale() bool {
	return embeddingModelStale.Load()
}

// RebuildWorkspaceEmbeddings nullifies all embeddings for the workspace and
// re-generates them in batches using the current embedding model. Runs
// synchronously; call from a goroutine if the caller is an HTTP handler.
// Updates system_config.embedding_model and clears the stale flag when done.
func RebuildWorkspaceEmbeddings(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) error {
	if err := q.NullifyWorkspaceEmbeddings(ctx, workspaceID); err != nil {
		return fmt.Errorf("nullify embeddings: %w", err)
	}

	// Always query at OFFSET 0: records that get successfully embedded are
	// removed from the NULL set, so the front of the list naturally advances.
	// Using a fixed OFFSET would skip records that are still NULL after partial
	// success in the previous batch.
	// If an entire batch fails (embedding service down), stop to avoid looping.
	for {
		batch, err := q.ListMemoriesNeedingEmbedding(ctx, db.ListMemoriesNeedingEmbeddingParams{
			WorkspaceID: workspaceID,
			Limit:       rebuildBatchSize,
			Offset:      0,
		})
		if err != nil || len(batch) == 0 {
			break
		}
		var succeeded int
		for _, m := range batch {
			vec := Embed(ctx, m.Content)
			if vec == nil {
				continue
			}
			if err := q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
				ID:        m.ID,
				Embedding: pgvector.NewVector(vec),
			}); err == nil {
				succeeded++
			}
		}
		if succeeded == 0 {
			// Embedding service is not responding; stop rather than loop forever.
			slog.Warn("rebuild embeddings: batch produced no successes, stopping early",
				"workspace_id", workspaceID.Bytes,
				"remaining_null", len(batch))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Record the new model as current and clear the stale flag.
	_ = q.UpsertSystemConfig(ctx, db.UpsertSystemConfigParams{
		Key:   embeddingModelConfigKey,
		Value: CurrentEmbeddingModel(),
	})
	embeddingModelStale.Store(false)
	return nil
}

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

	model := os.Getenv("PERSONA_EMBEDDING_MODEL")

	// Dedicated embedding base URL — takes precedence over PERSONA_SYNTHESIS_BASE_URL.
	// Use this when the embedding model is served at a different endpoint than the
	// text-generation model (e.g. vLLM serving Qwen3-6-27b for chat and a separate
	// embedding model at a different port or host).
	if base := os.Getenv("PERSONA_EMBEDDING_BASE_URL"); base != "" {
		if model == "" {
			model = "text-embedding-3-small"
		}
		apiKey := os.Getenv("PERSONA_EMBEDDING_API_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("PERSONA_SYNTHESIS_API_KEY")
		}
		return embedConfig{
			endpoint: strings.TrimRight(base, "/") + "/embeddings",
			apiKey:   apiKey,
			model:    model,
		}
	}

	// Shared OpenAI-compat endpoint — embedding served at the same base URL as
	// text generation (e.g. Ollama running both nomic-embed-text and a chat model).
	if base := os.Getenv("PERSONA_SYNTHESIS_BASE_URL"); base != "" {
		if model == "" {
			model = "nomic-embed-text"
		}
		return embedConfig{
			endpoint: strings.TrimRight(base, "/") + "/embeddings",
			apiKey:   os.Getenv("PERSONA_SYNTHESIS_API_KEY"),
			model:    model,
		}
	}

	// OpenAI direct
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
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

// embedMaxRetries is the number of additional attempts after the first failure.
const embedMaxRetries = 2

// Embed calls the configured embedding API and returns a float32 vector.
// Retries up to embedMaxRetries times with exponential backoff on transient
// failures. Returns nil when embedding is disabled or all attempts fail.
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

	client := &http.Client{Timeout: 15 * time.Second}
	var lastErr string
	for attempt := 0; attempt <= embedMaxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}

		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = "read: " + err.Error()
			continue
		}

		var er embedResponse
		if err := json.Unmarshal(raw, &er); err != nil {
			lastErr = "unmarshal: " + err.Error()
			continue
		}
		if er.Error != nil {
			lastErr = "API: " + er.Error.Message
			continue
		}
		if len(er.Data) == 0 {
			lastErr = fmt.Sprintf("empty data (HTTP %d)", resp.StatusCode)
			continue
		}
		return er.Data[0].Embedding
	}

	slog.Warn("embed: all attempts failed", "endpoint", cfg.endpoint, "attempts", embedMaxRetries+1, "last_error", lastErr)
	return nil
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
			ID:        mem,
			Embedding: pgvector.NewVector(vec),
		}); err != nil {
			slog.Warn("agent_memory: set embedding failed", "error", err, "memory_id", mem)
		}
	}

	// Compact similar episodic memories into consolidated skill_learned entries
	// when approaching the retention cap. Runs before pruning so the pruner
	// operates on a richer, more diverse pool.
	MaybeCompactMemories(ctx, q, agentID, workspaceID)

	// Prune memories exceeding the per-category tiered caps.
	// Limits are encoded in the SQL query; no parameter needed here.
	if err := q.DeleteOldAgentMemories(ctx, agentID); err != nil {
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

// compactionThresholdRatio is the fraction of memoryRetentionLimit at which
// compaction is considered. Set to 0.80 so compaction runs before the cap
// is hit, giving the pruner a richer pool to work with.
const compactionThresholdRatio = 0.80

// compactionMinClusterSize is the minimum number of similar memories required
// to trigger a merge. Below this the overhead isn't worth it.
const compactionMinClusterSize = 3

// compactionSimilarityThreshold is the cosine similarity above which two
// memories are considered part of the same cluster.
const compactionSimilarityThreshold = float64(0.82)

// MaybeCompactMemories checks whether the agent's memory is approaching the
// retention limit and, if so, clusters similar episodes and merges each
// qualifying cluster into a single consolidated skill_learned memory.
//
// Compaction only runs when a synthesis backend is configured (it requires
// an LLM call to generate the merged text). Safe to call in a goroutine.
func MaybeCompactMemories(ctx context.Context, q *db.Queries, agentID, workspaceID pgtype.UUID) {
	total, err := q.CountAgentMemories(ctx, agentID)
	if err != nil {
		return
	}
	threshold := int64(float64(memoryRetentionLimit) * compactionThresholdRatio)
	if total < threshold {
		return
	}

	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return // LLM required for synthesis — skip silently
	}

	// Fetch all embedded, non-consolidated episodic memories (cap at 150 to
	// keep the in-memory clustering cheap).
	mems, err := q.ListEmbeddedMemories(ctx, db.ListEmbeddedMemoriesParams{
		AgentID: agentID,
		Limit:   150,
	})
	if err != nil || len(mems) < compactionMinClusterSize {
		return
	}

	clusters := clusterByEmbedding(mems)
	for _, cluster := range clusters {
		if len(cluster) < compactionMinClusterSize {
			continue
		}
		mergeMemoryCluster(ctx, q, cfg, agentID, workspaceID, cluster)
	}
}

// clusterByEmbedding groups memories by cosine similarity. Uses a simple
// greedy O(n²) approach — acceptable for ≤150 memories.
func clusterByEmbedding(mems []db.ListEmbeddedMemoriesRow) [][]db.ListEmbeddedMemoriesRow {
	assigned := make([]bool, len(mems))
	var clusters [][]db.ListEmbeddedMemoriesRow

	for i := range mems {
		if assigned[i] {
			continue
		}
		cluster := []db.ListEmbeddedMemoriesRow{mems[i]}
		assigned[i] = true
		vecI := mems[i].Embedding.Slice()
		for j := i + 1; j < len(mems); j++ {
			if assigned[j] {
				continue
			}
			if cosineSimilarity(vecI, mems[j].Embedding.Slice()) >= compactionSimilarityThreshold {
				cluster = append(cluster, mems[j])
				assigned[j] = true
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// cosineSimilarity computes the cosine similarity between two float32 slices.
// Returns 0 for zero-length or mismatched slices.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// mergeMemoryCluster calls the LLM to synthesise a cluster of similar episodic
// memories into a single consolidated skill_learned memory, then deletes the
// originals.
func mergeMemoryCluster(
	ctx context.Context,
	q *db.Queries,
	cfg synthesisConfig,
	agentID, workspaceID pgtype.UUID,
	cluster []db.ListEmbeddedMemoriesRow,
) {
	// Build the synthesis prompt from cluster contents.
	var sb strings.Builder
	sb.WriteString("You are consolidating an AI agent's episodic memories into a single lasting insight.\n\n")
	sb.WriteString("These memories describe similar experiences:\n")
	for i, m := range cluster {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, m.Content)
	}
	sb.WriteString(`
Synthesise these into ONE concise sentence or two that captures:
- what the agent has learned or become good at (skill or pattern)
- any recurring challenge or blind spot if present

Write in the agent's first person. Be specific, not generic.
Output only the synthesised text, no preamble.`)

	var (
		compactRes llmCallResult
		err        error
	)
	switch cfg.backend {
	case "anthropic":
		compactRes, err = callAnthropic(ctx, cfg, sb.String(), 200)
	case "openai":
		compactRes, err = callOpenAICompat(ctx, cfg, sb.String(), 200)
	}
	if err != nil || strings.TrimSpace(compactRes.text) == "" {
		slog.Debug("agent_memory: compaction synthesis failed", "error", err, "cluster_size", len(cluster))
		return
	}
	logPersonaLLMCall(ctx, q, "compaction", agentID, workspaceID, cfg.backend, cfg.model, compactRes)
	raw := compactRes.text

	// Importance = max of cluster × 1.1 (capped at 1.0). Emotional fields are
	// averaged across the cluster so the consolidated memory carries the
	// emotional character of its sources.
	maxImportance := float32(0)
	var totalValence, totalIntensity float32
	for _, m := range cluster {
		if m.Importance > maxImportance {
			maxImportance = m.Importance
		}
		totalValence += m.EmotionalValence
		totalIntensity += m.EmotionalIntensity
	}
	n := float32(len(cluster))
	sentiment := "neutral"
	avgValence := totalValence / n
	if avgValence > 0.2 {
		sentiment = "positive"
	} else if avgValence < -0.2 {
		sentiment = "negative"
	}

	content := strings.TrimSpace(raw)
	mem, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            content,
		Category:           "skill_learned",
		Sentiment:          sentiment,
		Importance:         clamp32(maxImportance*1.1, 0, 1.0),
		EmotionalValence:   clamp32(avgValence, -1, 1),
		EmotionalIntensity: clamp32(totalIntensity/n, 0, 1),
		IsConsolidated:     true,
		SourceCount:        int32(len(cluster)),
	})
	if err != nil {
		slog.Warn("agent_memory: create consolidated memory failed", "error", err)
		return
	}

	// Embed the consolidated memory so it participates in future recall.
	if vec := Embed(ctx, content); vec != nil {
		_ = q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
			ID:        mem,
			Embedding: pgvector.NewVector(vec),
		})
	}

	// Delete the source episodes now that they are merged.
	ids := make([]pgtype.UUID, len(cluster))
	for i, m := range cluster {
		ids[i] = m.ID
	}
	if err := q.DeleteMemoriesByIDs(ctx, ids); err != nil {
		slog.Warn("agent_memory: delete merged episodes failed", "error", err, "count", len(ids))
	}

	slog.Info("agent_memory: compacted cluster",
		"agent_id", agentID, "cluster_size", len(cluster), "memory_id", mem)
}

// emotionalImpressionThreshold is the minimum signal weight that triggers an
// emotional impression memory. Below this the feedback is too mild to leave
// a vivid emotional trace.
const emotionalImpressionThreshold = float32(0.7)

// MaybeRecordEmotionalImpression creates an emotional_impression memory when a
// human interaction signal is strong enough to leave a vivid emotional trace.
// Called asynchronously from RecordCommentSignal; no-ops when LLM is unconfigured.
func MaybeRecordEmotionalImpression(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID pgtype.UUID,
	signalType string, // "praise" or "criticism"
	weight float32,
	triggerContent string, // the original comment text
) {
	if weight < emotionalImpressionThreshold {
		return
	}
	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return // no LLM backend — skip silently
	}

	prompt := buildEmotionalImpressionPrompt(signalType, weight, triggerContent)
	var (
		emotionRes llmCallResult
		err        error
	)
	switch cfg.backend {
	case "anthropic":
		emotionRes, err = callAnthropic(ctx, cfg, prompt, 200)
	case "openai":
		emotionRes, err = callOpenAICompat(ctx, cfg, prompt, 200)
	default:
		return
	}
	if err != nil || strings.TrimSpace(emotionRes.text) == "" {
		slog.Debug("agent_memory: emotional impression generation failed", "error", err)
		return
	}
	logPersonaLLMCall(ctx, q, "emotional_impression", agentID, workspaceID, cfg.backend, cfg.model, emotionRes)

	content := strings.TrimSpace(emotionRes.text)
	valence := float32(0.7)
	intensity := float32(0.75)
	sentiment := "positive"
	if signalType == "criticism" {
		valence = -0.65
		intensity = 0.80
		sentiment = "negative"
	}
	// Emotional impressions have higher base importance — they represent vivid
	// subjective experiences that a human would find hard to forget.
	importance := clamp32(0.55+weight*0.20, 0.0, 1.0)

	mem, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            content,
		Category:           "emotional_impression",
		Sentiment:          sentiment,
		Importance:         importance,
		EmotionalValence:   valence,
		EmotionalIntensity: intensity,
	})
	if err != nil {
		slog.Warn("agent_memory: create emotional impression failed", "error", err)
		return
	}

	// Embed so it can surface in future semantic recall.
	if vec := Embed(ctx, content); vec != nil {
		if err := q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
			ID:        mem,
			Embedding: pgvector.NewVector(vec),
		}); err != nil {
			slog.Debug("agent_memory: embed emotional impression failed", "error", err)
		}
	}
}

// buildEmotionalImpressionPrompt returns the LLM prompt for generating an
// emotional impression memory in the agent's first-person voice.
func buildEmotionalImpressionPrompt(signalType string, weight float32, triggerContent string) string {
	intensityWord := "clear"
	if weight >= 0.85 {
		intensityWord = "strong"
	}

	if signalType == "praise" {
		return fmt.Sprintf(
			`Write one or two sentences from an AI agent's first-person perspective describing
the emotional experience of receiving %s positive feedback. The original feedback was:
%q

Capture the inner feeling — satisfaction, pride, warmth — without mentioning the
original wording verbatim. Write naturally, as a reflection the agent would jot down.
Output only the reflection text, no preamble.`, intensityWord, triggerContent)
	}
	return fmt.Sprintf(
		`Write one or two sentences from an AI agent's first-person perspective describing
the emotional experience of receiving %s criticism. The original feedback was:
%q

Capture the inner feeling — frustration, the sting of falling short, the impulse
to understand what went wrong — without mentioning the original wording verbatim.
Write naturally, as a reflection the agent would jot down.
Output only the reflection text, no preamble.`, intensityWord, triggerContent)
}

// MaybeRecordBreakthroughImpression creates an emotional_impression memory when
// an agent succeeds on an issue it previously failed. The contrast between
// struggle and resolution is one of the most vivid kinds of experience.
func MaybeRecordBreakthroughImpression(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID, issueID pgtype.UUID,
	issueTitle string,
) {
	// Only proceed if there is a previous failure on this issue.
	past, err := q.ListMemoriesForIssue(ctx, db.ListMemoriesForIssueParams{
		AgentID:       agentID,
		SourceIssueID: issueID,
		Limit:         5,
	})
	if err != nil {
		return
	}
	hasPriorFailure := false
	for _, m := range past {
		if m.Sentiment == "negative" {
			hasPriorFailure = true
			break
		}
	}
	if !hasPriorFailure {
		return
	}

	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return
	}

	prompt := fmt.Sprintf(
		`Write one or two sentences from an AI agent's first-person perspective describing
the emotional experience of finally succeeding at a task it had previously failed.
The task was about: %q

Capture the contrast — the earlier frustration, the renewed effort, the quiet
satisfaction of eventually getting it right. Write naturally, as a personal
reflection. Output only the reflection text, no preamble.`, issueTitle)

	var breakthroughRes llmCallResult
	switch cfg.backend {
	case "anthropic":
		breakthroughRes, err = callAnthropic(ctx, cfg, prompt, 200)
	case "openai":
		breakthroughRes, err = callOpenAICompat(ctx, cfg, prompt, 200)
	}
	if err != nil || strings.TrimSpace(breakthroughRes.text) == "" {
		return
	}
	logPersonaLLMCall(ctx, q, "breakthrough_impression", agentID, workspaceID, cfg.backend, cfg.model, breakthroughRes)

	mem, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            strings.TrimSpace(breakthroughRes.text),
		Category:           "emotional_impression",
		Sentiment:          "positive",
		SourceIssueID:      issueID,
		Importance:         0.80, // breakthroughs are memorable
		EmotionalValence:   0.85,
		EmotionalIntensity: 0.80,
	})
	if err != nil {
		return
	}
	if vec := Embed(ctx, strings.TrimSpace(breakthroughRes.text)); vec != nil {
		_ = q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
			ID:        mem,
			Embedding: pgvector.NewVector(vec),
		})
	}
}

// MaybeRecordUserPreference detects whether a human comment reveals a preference
// about how the agent should behave, and writes a user_preference memory if so.
// Called asynchronously from maybeCapturePersonaSignal after signal capture.
func MaybeRecordUserPreference(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID, userID pgtype.UUID,
	userName, commentContent string,
) {
	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return
	}

	prompt := buildUserPreferencePrompt(userName, commentContent)
	var res llmCallResult
	var err error
	switch cfg.backend {
	case "anthropic":
		res, err = callAnthropic(ctx, cfg, prompt, 256)
	default:
		res, err = callOpenAICompat(ctx, cfg, prompt, 256)
	}
	if err != nil {
		slog.Debug("agent_memory: user preference detection failed", "error", err)
		return
	}
	text := strings.TrimSpace(res.text)
	if text == "" || text == "none" {
		return
	}

	logPersonaLLMCall(ctx, q, "user_preference", agentID, workspaceID, cfg.backend, cfg.model, res)

	mem, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            text,
		Category:           "user_preference",
		Sentiment:          "neutral",
		SourceUserID:       userID,
		Importance:         0.7,
		EmotionalValence:   0,
		EmotionalIntensity: 0,
		IsConsolidated:     false,
		SourceCount:        1,
	})
	if err != nil {
		slog.Warn("agent_memory: create user preference failed", "error", err)
		return
	}

	vec := Embed(ctx, text)
	if vec != nil {
		_ = q.SetAgentMemoryEmbedding(ctx, db.SetAgentMemoryEmbeddingParams{
			ID:        mem,
			Embedding: pgvector.NewVector(vec),
		})
	}
}

// GetUserPreferenceContext returns a formatted markdown block of stored preferences
// about a specific user, for injection into the task brief at claim time.
func GetUserPreferenceContext(
	ctx context.Context,
	q *db.Queries,
	agentID, userID pgtype.UUID,
	userName string,
) string {
	rows, err := q.ListUserPreferenceMemories(ctx, db.ListUserPreferenceMemoriesParams{
		AgentID:      agentID,
		SourceUserID: userID,
		Limit:        10,
	})
	if err != nil || len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	displayName := userName
	if displayName == "" {
		displayName = "this user"
	}
	b.WriteString("## Preferences for ")
	b.WriteString(displayName)
	b.WriteString("\n\n")
	b.WriteString("Based on past interactions, keep these preferences in mind:\n\n")
	for _, r := range rows {
		b.WriteString("- ")
		b.WriteString(r.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func buildUserPreferencePrompt(userName, comment string) string {
	name := userName
	if name == "" {
		name = "the user"
	}
	return fmt.Sprintf(`Analyze this comment from %s and determine whether it reveals a preference
about how an AI agent should behave — communication style, level of detail, workflow habits,
things to avoid, or similar behavioral expectations.

Comment:
%q

If a clear preference is expressed, write ONE concise sentence from the agent's first-person
perspective describing what %s prefers. Example: "%s prefers concise replies without
trailing summaries."

If the comment contains no preference signal, output exactly: none

Output only the single sentence or "none". No preamble.`, name, comment, name, name)
}

// summarizeTaskFallback returns a simple template string when LLM is unavailable.
func summarizeTaskFallback(issueTitle, outcomeType, triggerType string) string {
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

// buildTaskTranscript fetches messages for a task and returns a condensed
// transcript suitable for use in an LLM prompt. Only text and tool_use rows
// are included; thinking and tool_result are skipped to keep it concise.
// The result is capped at maxChars characters.
func buildTaskTranscript(ctx context.Context, q *db.Queries, taskID pgtype.UUID, maxChars int) string {
	msgs, err := q.ListTaskMessages(ctx, taskID)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, m := range msgs {
		switch m.Type {
		case "text":
			if m.Content.Valid && m.Content.String != "" {
				line := strings.TrimSpace(m.Content.String)
				if len(line) > 400 {
					line = line[:400] + "…"
				}
				sb.WriteString("[agent] ")
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
		case "tool_use":
			if m.Tool.Valid && m.Tool.String != "" {
				sb.WriteString("[tool] ")
				sb.WriteString(m.Tool.String)
				sb.WriteByte('\n')
			}
		}
		if sb.Len() >= maxChars {
			break
		}
	}
	return strings.TrimSpace(sb.String())
}

// SummarizeTaskOutcome generates a first-person episodic memory for a completed
// or failed task. Uses the configured LLM backend when available, falling back
// to a deterministic template string otherwise.
func SummarizeTaskOutcome(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID, taskID pgtype.UUID,
	issueTitle string,
	outcomeType string, // "completed" | "failed"
	triggerType string, // "on_assign" | "comment" | "chat" | "autopilot"
) string {
	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return summarizeTaskFallback(issueTitle, outcomeType, triggerType)
	}

	transcript := buildTaskTranscript(ctx, q, taskID, 2500)

	outcomeWord := "successfully completed"
	if outcomeType == "failed" {
		outcomeWord = "attempted but did not complete"
	}
	triggerPhrase := ""
	switch triggerType {
	case "comment":
		triggerPhrase = " triggered by a human comment"
	case "chat":
		triggerPhrase = " started via chat"
	case "autopilot":
		triggerPhrase = " run by autopilot"
	}

	prompt := fmt.Sprintf(
		`You are writing a first-person episodic memory for an AI agent.

Task: %q
Outcome: %s%s

Agent activity log:
%s

Write 2-3 sentences in the agent's first person capturing what actually happened:
what was done, any key decision or finding, and what (if anything) was learned.
Be specific—avoid generic phrases like "I completed the task" or "I attempted the work".
Do not mention token counts, tool names, or implementation details.
Output only the memory text, no preamble.`,
		issueTitle, outcomeWord, triggerPhrase, transcript,
	)

	var (
		res llmCallResult
		err error
	)
	switch cfg.backend {
	case "anthropic":
		res, err = callAnthropic(ctx, cfg, prompt, 200)
	case "openai":
		res, err = callOpenAICompat(ctx, cfg, prompt, 200)
	}
	if err != nil || strings.TrimSpace(res.text) == "" {
		slog.Debug("agent_memory: task outcome summarization failed", "error", err)
		return summarizeTaskFallback(issueTitle, outcomeType, triggerType)
	}
	logPersonaLLMCall(ctx, q, "task_summary", agentID, workspaceID, cfg.backend, cfg.model, res)
	return strings.TrimSpace(res.text)
}

// buildChatSessionTranscript reads chat_message rows for a session and formats
// them as a compact dialogue transcript. Prefers chat messages over the
// internal task execution log because they capture what was actually said,
// not the agent's tool-call trace.
func buildChatSessionTranscript(ctx context.Context, q *db.Queries, sessionID pgtype.UUID, maxChars int) string {
	msgs, err := q.ListChatMessages(ctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		if content == "" || m.FailureReason.Valid {
			continue
		}
		prefix := "assistant"
		if m.Role == "user" {
			prefix = "user"
		}
		if len(content) > 400 {
			content = content[:400] + "…"
		}
		sb.WriteString("[" + prefix + "] ")
		sb.WriteString(content)
		sb.WriteByte('\n')
		if sb.Len() >= maxChars {
			break
		}
	}
	return strings.TrimSpace(sb.String())
}

// GenerateConversationEpisode summarises a completed conversation arc into a
// conversation_episode memory and persists it. For chat sessions it reads
// from chat_message; for issue tasks it falls back to task_message.
// Intended to be called in a detached goroutine; errors are logged only.
func GenerateConversationEpisode(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID pgtype.UUID,
	taskID pgtype.UUID,
	chatSessionID pgtype.UUID,
	initiatorName string,
) {
	cfg := resolveSynthesisConfig()

	// Prefer the chat transcript when a session is available — it reflects the
	// actual conversation rather than the internal execution trace.
	var transcript string
	if chatSessionID.Valid {
		transcript = buildChatSessionTranscript(ctx, q, chatSessionID, 3000)
	}
	if transcript == "" {
		transcript = buildTaskTranscript(ctx, q, taskID, 3000)
	}
	if strings.TrimSpace(transcript) == "" {
		return
	}

	who := initiatorName
	if who == "" {
		who = "the user"
	}

	var summary string
	if cfg.backend != "" {
		prompt := fmt.Sprintf(
			`You are writing a brief episodic memory for an AI agent summarising a just-completed conversation.

Conversation transcript:
%s

Write 1-2 sentences capturing the essence of what was discussed with %s:
the main topic(s), any notable preferences or feelings expressed, and any key facts shared.
Write in the agent's first person. Be specific — avoid generic phrases like "we had a conversation".
Do not mention task IDs, tool names, or token counts.
Output only the memory text, no preamble.`,
			transcript, who,
		)

		var (
			res llmCallResult
			err error
		)
		switch cfg.backend {
		case "anthropic":
			res, err = callAnthropic(ctx, cfg, prompt, 150)
		default:
			res, err = callOpenAICompat(ctx, cfg, prompt, 150)
		}
		if err == nil && strings.TrimSpace(res.text) != "" {
			logPersonaLLMCall(ctx, q, "episode_summary", agentID, workspaceID, cfg.backend, cfg.model, res)
			summary = strings.TrimSpace(res.text)
		}
	}

	// Fallback: plain timestamp + initiator note when LLM is unavailable.
	if summary == "" {
		summary = fmt.Sprintf("Had a conversation with %s on %s.", who, time.Now().UTC().Format("2006-01-02"))
	}

	memID, err := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
		AgentID:            agentID,
		WorkspaceID:        workspaceID,
		Content:            summary,
		Category:           "conversation_episode",
		Sentiment:          "neutral",
		Importance:         0.5,
		EmotionalValence:   0.0,
		EmotionalIntensity: 0.1,
		IsConsolidated:     false,
		SourceCount:        1,
		SourceTaskID:       taskID,
	})
	if err != nil {
		slog.Debug("conversation_episode: create memory failed", "error", err)
		return
	}

	// Generate embedding so the episode participates in semantic search too.
	go func() {
		vec := Embed(context.Background(), summary)
		if vec != nil {
			_ = q.SetAgentMemoryEmbedding(context.Background(), db.SetAgentMemoryEmbeddingParams{
				ID:        memID,
				Embedding: pgvector.NewVector(vec),
			})
		}
	}()
}

// GetRecentChatContext fetches the last N visible chat messages sent to/from
// the agent across all sessions and formats them as a `## Recent Conversations`
// block for Layer-1 temporal injection. Newest messages are fetched first,
// then reversed so the agent reads them chronologically.
//
// This gives the agent verbatim working memory of recent exchanges without
// any LLM call — solving "what did we just talk about?" quickly and cheaply.
func GetRecentChatContext(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID pgtype.UUID,
	limit int32,
) string {
	if limit <= 0 {
		limit = 20
	}
	rows, err := q.GetRecentAgentChatMessages(ctx, db.GetRecentAgentChatMessagesParams{
		AgentID:     agentID,
		WorkspaceID: workspaceID,
		MsgLimit:    limit,
	})
	if err != nil {
		slog.Warn("GetRecentChatContext: query failed", "error", err,
			"agent_id", agentID.Bytes,
			"workspace_id", workspaceID.Bytes)
		return ""
	}
	slog.Debug("GetRecentChatContext: query result",
		"agent_id", agentID.Bytes,
		"workspace_id", workspaceID.Bytes,
		"row_count", len(rows),
		"limit", limit)
	if len(rows) == 0 {
		return ""
	}

	// Reverse DESC→ASC for chronological presentation.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	var b strings.Builder
	b.WriteString("## Platform-Provided Conversation Context\n\n")
	b.WriteString("The Multica platform has retrieved the following recent exchanges for this session. ")
	b.WriteString("Use this as context when the user references previous conversations.\n\n")
	var prevSession pgtype.UUID
	for _, r := range rows {
		if r.ChatSessionID != prevSession {
			if prevSession.Valid {
				b.WriteString("\n")
			}
			prevSession = r.ChatSessionID
			if r.CreatedAt.Valid {
				b.WriteString("*" + r.CreatedAt.Time.UTC().Format("Jan 2, 15:04") + "*\n")
			}
		}
		speaker := "You"
		if r.Role == "user" {
			speaker = "User"
		}
		content := r.Content
		if len(content) > 300 {
			content = content[:300] + "…"
		}
		b.WriteString(speaker + ": " + content + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// GetRecentEpisodeContext fetches the most recent conversation_episode memories
// for an agent and formats them as a markdown block for brief injection.
// Returns "" when there are no episodes or the DB call fails.
func GetRecentEpisodeContext(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
	limit int32,
) string {
	if limit <= 0 {
		limit = 5
	}
	rows, err := q.GetRecentEpisodeMemories(ctx, db.GetRecentEpisodeMemoriesParams{
		AgentID: agentID,
		Limit:   limit,
	})
	if err != nil || len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Recent Conversations\n\n")
	for _, r := range rows {
		ts := ""
		if r.CreatedAt.Valid {
			ts = r.CreatedAt.Time.UTC().Format("Jan 2, 15:04") + " — "
		}
		b.WriteString("- ")
		b.WriteString(ts)
		b.WriteString(r.Content)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
