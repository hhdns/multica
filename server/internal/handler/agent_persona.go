package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// AgentPersonaResponse is the wire shape for GET /api/agents/{id}/persona.
type AgentPersonaResponse struct {
	AgentID            string                         `json:"agent_id"`
	TraitThoroughness  int32                          `json:"trait_thoroughness"`
	TraitVerbosity     int32                          `json:"trait_verbosity"`
	TraitRiskAppetite  int32                          `json:"trait_risk_appetite"`
	TraitCuriosity     int32                          `json:"trait_curiosity"`
	TraitConfidence    int32                          `json:"trait_confidence"`
	Strengths          []string                       `json:"strengths"`
	BlindSpots         []string                       `json:"blind_spots"`
	Mood               string                         `json:"mood"`
	MoodUpdatedAt      string                         `json:"mood_updated_at"`
	VarianceLevel      int32                          `json:"variance_level"`
	Identity           *string                        `json:"identity"`
	SignalCount        int32                          `json:"signal_count"`
	LastSynthesizedAt  *string                        `json:"last_synthesized_at"`
	RecentSignals      []AgentInteractionSignalResponse `json:"recent_signals"`
	CreatedAt          string                         `json:"created_at"`
	UpdatedAt          string                         `json:"updated_at"`
}

// AgentInteractionSignalResponse is the wire shape for a single signal.
type AgentInteractionSignalResponse struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Content      string  `json:"content"`
	Weight       float32 `json:"weight"`
	SourceType   string  `json:"source_type"`
	SourceUserID *string `json:"source_user_id"`
	CreatedAt    string  `json:"created_at"`
}

// UpdateAgentPersonaRequest is the wire shape for PUT /api/agents/{id}/persona.
type UpdateAgentPersonaRequest struct {
	TraitThoroughness  *int32   `json:"trait_thoroughness"`
	TraitVerbosity     *int32   `json:"trait_verbosity"`
	TraitRiskAppetite  *int32   `json:"trait_risk_appetite"`
	TraitCuriosity     *int32   `json:"trait_curiosity"`
	TraitConfidence    *int32   `json:"trait_confidence"`
	Strengths          []string `json:"strengths"`
	BlindSpots         []string `json:"blind_spots"`
	VarianceLevel      *int32   `json:"variance_level"`
	Identity           *string  `json:"identity"`
}

func personaToResponse(p db.AgentPersona, signals []db.AgentInteractionSignal) AgentPersonaResponse {
	resp := AgentPersonaResponse{
		AgentID:           uuidToString(p.AgentID),
		TraitThoroughness: p.TraitThoroughness,
		TraitVerbosity:    p.TraitVerbosity,
		TraitRiskAppetite: p.TraitRiskAppetite,
		TraitCuriosity:    p.TraitCuriosity,
		TraitConfidence:   p.TraitConfidence,
		Strengths:         p.Strengths,
		BlindSpots:        p.BlindSpots,
		Mood:              p.Mood,
		MoodUpdatedAt:     p.MoodUpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		VarianceLevel:     p.VarianceLevel,
		SignalCount:       p.SignalCount,
		CreatedAt:         p.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         p.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		RecentSignals:     make([]AgentInteractionSignalResponse, 0, len(signals)),
	}
	if p.Identity.Valid {
		v := p.Identity.String
		resp.Identity = &v
	}
	if p.LastSynthesizedAt.Valid {
		v := p.LastSynthesizedAt.Time.Format("2006-01-02T15:04:05Z07:00")
		resp.LastSynthesizedAt = &v
	}
	for _, s := range signals {
		sr := AgentInteractionSignalResponse{
			ID:         uuidToString(s.ID),
			Type:       s.Type,
			Content:    s.Content,
			Weight:     s.Weight,
			SourceType: s.SourceType,
			CreatedAt:  s.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		}
		if s.SourceUserID.Valid {
			v := uuidToString(s.SourceUserID)
			sr.SourceUserID = &v
		}
		resp.RecentSignals = append(resp.RecentSignals, sr)
	}
	return resp
}

// GetAgentPersona handles GET /api/agents/{id}/persona.
// Creates a default persona row if none exists yet.
func (h *Handler) GetAgentPersona(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}

	ctx := r.Context()

	persona, err := h.Queries.UpsertAgentPersona(ctx, db.UpsertAgentPersonaParams{
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil {
		slog.Warn("get agent persona: upsert failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "failed to load persona")
		return
	}

	signals, err := h.Queries.ListAgentInteractionSignals(ctx, db.ListAgentInteractionSignalsParams{
		AgentID: agent.ID,
		Limit:   20,
	})
	if err != nil {
		slog.Warn("get agent persona: list signals failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		signals = nil
	}

	writeJSON(w, http.StatusOK, personaToResponse(persona, signals))
}

// UpdateAgentPersona handles PUT /api/agents/{id}/persona.
func (h *Handler) UpdateAgentPersona(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}

	var req UpdateAgentPersonaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()

	// Load current values so we can merge (partial update pattern).
	current, err := h.Queries.UpsertAgentPersona(ctx, db.UpsertAgentPersonaParams{
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil {
		slog.Warn("update agent persona: upsert failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "failed to load persona")
		return
	}

	// Merge request fields over current values.
	thoroughness := current.TraitThoroughness
	verbosity := current.TraitVerbosity
	riskAppetite := current.TraitRiskAppetite
	curiosity := current.TraitCuriosity
	confidence := current.TraitConfidence
	strengths := current.Strengths
	blindSpots := current.BlindSpots
	varianceLevel := current.VarianceLevel
	identity := current.Identity

	if req.TraitThoroughness != nil {
		thoroughness = *req.TraitThoroughness
	}
	if req.TraitVerbosity != nil {
		verbosity = *req.TraitVerbosity
	}
	if req.TraitRiskAppetite != nil {
		riskAppetite = *req.TraitRiskAppetite
	}
	if req.TraitCuriosity != nil {
		curiosity = *req.TraitCuriosity
	}
	if req.TraitConfidence != nil {
		confidence = *req.TraitConfidence
	}
	if req.Strengths != nil {
		strengths = req.Strengths
	}
	if req.BlindSpots != nil {
		blindSpots = req.BlindSpots
	}
	if req.VarianceLevel != nil {
		varianceLevel = *req.VarianceLevel
	}
	if req.Identity != nil {
		identity = pgtype.Text{String: *req.Identity, Valid: true}
	}

	updated, err := h.Queries.UpdateAgentPersona(ctx, db.UpdateAgentPersonaParams{
		AgentID:           agent.ID,
		TraitThoroughness: thoroughness,
		TraitVerbosity:    verbosity,
		TraitRiskAppetite: riskAppetite,
		TraitCuriosity:    curiosity,
		TraitConfidence:   confidence,
		Strengths:         strengths,
		BlindSpots:        blindSpots,
		VarianceLevel:     varianceLevel,
		Identity:          identity,
	})
	if err != nil {
		slog.Warn("update agent persona: update failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "failed to update persona")
		return
	}

	signals, err := h.Queries.ListAgentInteractionSignals(ctx, db.ListAgentInteractionSignalsParams{
		AgentID: agent.ID,
		Limit:   20,
	})
	if err != nil {
		signals = nil
	}

	slog.Info("agent persona updated",
		append(logger.RequestAttrs(r), "agent_id", agentID)...)
	writeJSON(w, http.StatusOK, personaToResponse(updated, signals))
}

// formatPersonaContext returns a compact natural-language blurb describing the
// agent's current persona state for injection into the task runtime brief.
// It is intentionally short (2-4 lines) so it doesn't bloat the prompt.
func formatPersonaContext(p db.AgentPersona) string {
	var b strings.Builder

	// Mood-based behavioural nudge.
	switch p.Mood {
	case "energized":
		b.WriteString("You're in a high-energy state with recent successes behind you. Trust your instincts, move decisively, and tackle ambitious tasks with confidence.")
	case "cautious":
		b.WriteString("You've encountered some setbacks recently. Be deliberate, double-check your work, and prefer smaller, safer steps over bold moves.")
	case "playful":
		b.WriteString("You're feeling creative and spontaneous today. Bring originality to your approach — there's room to explore unconventional solutions.")
	default: // calm
		b.WriteString("You're in a steady, balanced state. Work methodically and adapt your approach to what each task needs.")
	}

	// Spontaneity hint only at the extremes to avoid noise.
	if p.VarianceLevel >= 70 {
		b.WriteString(" Occasional creative flourishes and unexpected angles are welcome — lean into your spontaneous side when context allows.")
	} else if p.VarianceLevel <= 20 {
		b.WriteString(" Stay consistent and predictable — your current mode values reliability over novelty.")
	}

	return b.String()
}

// SynthesizeAgentPersona handles POST /api/agents/{id}/persona/synthesize.
// It calls Claude Haiku to regenerate agent.instructions from the current
// persona data. Requires agent-management permission. Returns 202 Accepted
// and runs the synthesis synchronously (it's fast; ~1–2 s on Haiku).
func (h *Handler) SynthesizeAgentPersona(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}

	ctx := r.Context()
	if err := service.SynthesizeAgentInstructions(
		ctx, h.Queries, agent.ID, agent.WorkspaceID,
		agent.Name,
		agent.Instructions,
	); err != nil {
		slog.Warn("synthesize persona: failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "synthesis failed: "+err.Error())
		return
	}

	slog.Info("agent instructions synthesized from persona",
		append(logger.RequestAttrs(r), "agent_id", agentID)...)

	// Return the fresh persona so the UI can update last_synthesized_at.
	persona, err := h.Queries.GetAgentPersona(ctx, agent.ID)
	if err != nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	signals, _ := h.Queries.ListAgentInteractionSignals(ctx, db.ListAgentInteractionSignalsParams{
		AgentID: agent.ID,
		Limit:   20,
	})
	writeJSON(w, http.StatusAccepted, personaToResponse(persona, signals))
}

// AgentMemoryResponse is the wire shape for a single memory entry.
type AgentMemoryResponse struct {
	ID             string  `json:"id"`
	Content        string  `json:"content"`
	Category       string  `json:"category"`
	Sentiment      string  `json:"sentiment"`
	Importance     float32 `json:"importance"`
	HasEmbedding   bool    `json:"has_embedding"`
	IsConsolidated bool    `json:"is_consolidated"`
	SourceCount    int32   `json:"source_count"`
	SourceIssueID  *string `json:"source_issue_id,omitempty"`
	CreatedAt      string  `json:"created_at"`
}

// ListAgentMemories handles GET /api/agents/{id}/memories.
// Returns the most recent memories for debug purposes.
func (h *Handler) ListAgentMemories(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}

	memories, err := h.Queries.ListAgentMemories(r.Context(), db.ListAgentMemoriesParams{
		AgentID: agent.ID,
		Limit:   50,
	})
	if err != nil {
		slog.Warn("list agent memories: query failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "failed to list memories")
		return
	}

	out := make([]AgentMemoryResponse, 0, len(memories))
	for _, m := range memories {
		r2 := AgentMemoryResponse{
			ID:             uuidToString(m.ID),
			Content:        m.Content,
			Category:       m.Category,
			Sentiment:      m.Sentiment,
			Importance:     m.Importance,
			HasEmbedding:   m.HasEmbedding,
			IsConsolidated: m.IsConsolidated,
			SourceCount:    m.SourceCount,
			CreatedAt:      m.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		}
		if m.SourceIssueID.Valid {
			v := uuidToString(m.SourceIssueID)
			r2.SourceIssueID = &v
		}
		out = append(out, r2)
	}
	writeJSON(w, http.StatusOK, out)
}

// PersonaLLMCallResponse is the wire shape for a single LLM call record.
type PersonaLLMCallResponse struct {
	CallType     string `json:"call_type"`
	Backend      string `json:"backend"`
	Model        string `json:"model"`
	InputTokens  int32  `json:"input_tokens"`
	OutputTokens int32  `json:"output_tokens"`
	LatencyMs    int32  `json:"latency_ms"`
	CreatedAt    string `json:"created_at"`
}

// ListAgentLLMCalls handles GET /api/agents/{id}/llm-calls.
func (h *Handler) ListAgentLLMCalls(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}

	calls, err := h.Queries.ListAgentLLMCalls(r.Context(), db.ListAgentLLMCallsParams{
		AgentID: agent.ID,
		Limit:   100,
	})
	if err != nil {
		slog.Warn("list agent llm calls: query failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", agentID)...)
		writeError(w, http.StatusInternalServerError, "failed to list llm calls")
		return
	}

	out := make([]PersonaLLMCallResponse, 0, len(calls))
	for _, c := range calls {
		out = append(out, PersonaLLMCallResponse{
			CallType:     c.CallType,
			Backend:      c.Backend,
			Model:        c.Model,
			InputTokens:  c.InputTokens,
			OutputTokens: c.OutputTokens,
			LatencyMs:    c.LatencyMs,
			CreatedAt:    c.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// RecordTaskSignal writes a system-sourced interaction signal after task completion/failure.
func RecordTaskSignal(ctx context.Context, q *db.Queries, agentID, workspaceID pgtype.UUID, signalType, content string) {
	service.RecordCommentSignal(ctx, q, agentID, workspaceID, signalType, 0.5, content, pgtype.UUID{}, pgtype.UUID{})
}

// ExportAgentPersona handles GET /api/agents/{id}/persona/export.
// Returns the full persona bundle as a downloadable JSON file.
func (h *Handler) ExportAgentPersona(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	bundle, err := service.ExportPersona(r.Context(), h.Queries, agent)
	if err != nil {
		slog.Warn("export persona: failed", "agent_id", uuidToString(agent.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "export failed")
		return
	}

	filename := fmt.Sprintf("%s-persona.json", strings.ReplaceAll(agent.Name, " ", "-"))
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bundle)
}

// importPersonaRequest is the wire shape for POST /api/agents/{id}/persona/import.
// UserMappings maps source email addresses (from the bundle) to local user UUIDs,
// allowing the caller to explicitly reassign user_preference memories to workspace members.
type importPersonaRequest struct {
	Bundle       service.PersonaBundle `json:"bundle"`
	UserMappings map[string]string     `json:"user_mappings,omitempty"`
}

// ImportAgentPersona handles POST /api/agents/{id}/persona/import.
// Merges a PersonaBundle into the agent: overwrites instructions/traits, appends new memories.
func (h *Handler) ImportAgentPersona(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	var req importPersonaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Bundle.Version != "1" {
		writeError(w, http.StatusBadRequest, "unsupported bundle version")
		return
	}
	if req.UserMappings == nil {
		req.UserMappings = map[string]string{}
	}

	imported, skipped, err := service.ImportPersona(r.Context(), h.Queries, agent, req.Bundle, req.UserMappings)
	if err != nil {
		slog.Warn("import persona: failed", "agent_id", uuidToString(agent.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "import failed")
		return
	}

	slog.Info("persona imported", "agent_id", uuidToString(agent.ID),
		"memories_imported", imported, "memories_skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"memories_imported": imported,
		"memories_skipped":  skipped,
	})
}

// RebuildEmbeddings handles POST /api/workspaces/{id}/memories/rebuild-embeddings.
// Nullifies all memory embeddings for the workspace and re-generates them with
// the current embedding model. Returns 202 immediately; rebuild runs in a goroutine.
func (h *Handler) RebuildEmbeddings(w http.ResponseWriter, r *http.Request) {
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	wsIDStr := uuidToString(wsID)
	go func() {
		ctx := context.Background()
		if err := service.RebuildWorkspaceEmbeddings(ctx, h.Queries, wsID); err != nil {
			slog.Warn("rebuild embeddings: failed", "workspace_id", wsIDStr, "error", err)
			return
		}
		slog.Info("rebuild embeddings: completed", "workspace_id", wsIDStr)
		h.publish(protocol.EventMemoryEmbeddingsRebuilt, wsIDStr, "system", "", map[string]any{
			"workspace_id": wsIDStr,
		})
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}
