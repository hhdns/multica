package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const personaBundleVersion = "1"

// PersonaBundle is the portable export format for an agent's full persona state.
// It is designed to be self-contained: another system (or a fresh Multica install)
// can restore the agent's personality and memories from this file alone.
type PersonaBundle struct {
	Version    string    `json:"version"`
	ExportedAt time.Time `json:"exported_at"`

	Agent   BundleAgent   `json:"agent"`
	Persona BundlePersona `json:"persona"`
	Memories []BundleMemory `json:"memories"`

	// NarrativeSummary is a plain-language description of the agent's personality
	// and key experiences. It is suitable for use as a system prompt seed in any
	// LLM-based agent framework, not just Multica.
	NarrativeSummary string `json:"narrative_summary,omitempty"`
}

type BundleAgent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type BundlePersona struct {
	Instructions     string   `json:"instructions"`
	Mood             string   `json:"mood"`
	TraitThoroughness int32   `json:"trait_thoroughness"`
	TraitVerbosity   int32    `json:"trait_verbosity"`
	TraitRiskAppetite int32   `json:"trait_risk_appetite"`
	TraitCuriosity   int32    `json:"trait_curiosity"`
	TraitConfidence  int32    `json:"trait_confidence"`
	VarianceLevel    int32    `json:"variance_level"`
	Strengths        []string `json:"strengths"`
	BlindSpots       []string `json:"blind_spots"`
	Identity         string   `json:"identity,omitempty"`
}

type BundleMemory struct {
	Content            string    `json:"content"`
	Category           string    `json:"category"`
	Sentiment          string    `json:"sentiment"`
	Importance         float32   `json:"importance"`
	EmotionalValence   float32   `json:"emotional_valence"`
	EmotionalIntensity float32   `json:"emotional_intensity"`
	IsConsolidated     bool      `json:"is_consolidated"`
	SourceCount        int32     `json:"source_count"`
	CreatedAt          time.Time `json:"created_at"`
	// SourceUserName and SourceUserEmail are set for user_preference memories.
	// On import, SourceUserEmail is used to resolve the local user ID.
	SourceUserName  string `json:"source_user_name,omitempty"`
	SourceUserEmail string `json:"source_user_email,omitempty"`
}

// ExportPersona assembles a PersonaBundle for an agent, including an LLM-generated
// narrative summary. The bundle is suitable for backup, migration, or "reincarnation"
// in another agent orchestration system.
func ExportPersona(
	ctx context.Context,
	q *db.Queries,
	agent db.Agent,
) (PersonaBundle, error) {
	bundle := PersonaBundle{
		Version:    personaBundleVersion,
		ExportedAt: time.Now().UTC(),
		Agent: BundleAgent{
			Name:        agent.Name,
			Description: agent.Description,
		},
		Persona: BundlePersona{
			Instructions: agent.Instructions,
		},
	}

	// Persona traits.
	if persona, err := q.GetAgentPersona(ctx, agent.ID); err == nil {
		bundle.Persona.Mood = persona.Mood
		bundle.Persona.TraitThoroughness = persona.TraitThoroughness
		bundle.Persona.TraitVerbosity = persona.TraitVerbosity
		bundle.Persona.TraitRiskAppetite = persona.TraitRiskAppetite
		bundle.Persona.TraitCuriosity = persona.TraitCuriosity
		bundle.Persona.TraitConfidence = persona.TraitConfidence
		bundle.Persona.VarianceLevel = persona.VarianceLevel
		bundle.Persona.Strengths = persona.Strengths
		bundle.Persona.BlindSpots = persona.BlindSpots
		if persona.Identity.Valid {
			bundle.Persona.Identity = persona.Identity.String
		}
	}

	// Memories (embeddings excluded — model-specific, rebuilt on import).
	rows, err := q.ExportAgentMemories(ctx, agent.ID)
	if err != nil {
		return PersonaBundle{}, fmt.Errorf("export memories: %w", err)
	}
	bundle.Memories = make([]BundleMemory, 0, len(rows))
	for _, r := range rows {
		m := BundleMemory{
			Content:            r.Content,
			Category:           r.Category,
			Sentiment:          r.Sentiment,
			Importance:         r.Importance,
			EmotionalValence:   r.EmotionalValence,
			EmotionalIntensity: r.EmotionalIntensity,
			IsConsolidated:     r.IsConsolidated,
			SourceCount:        r.SourceCount,
		}
		if r.CreatedAt.Valid {
			m.CreatedAt = r.CreatedAt.Time.UTC()
		}
		if r.SourceUserName.Valid {
			m.SourceUserName = r.SourceUserName.String
		}
		if r.SourceUserEmail.Valid {
			m.SourceUserEmail = r.SourceUserEmail.String
		}
		bundle.Memories = append(bundle.Memories, m)
	}

	// Generate narrative summary via LLM (non-blocking failure).
	bundle.NarrativeSummary = generateNarrativeSummary(ctx, bundle)

	return bundle, nil
}

// ImportPersona merges a PersonaBundle into an existing agent. Behaviour:
//   - Persona instructions and traits are overwritten with bundle values.
//   - Memories are appended; identical content is skipped (dedup by content).
//   - user_preference memories: source_user_email is resolved to a local user ID;
//     unmatched users keep source_user_id = NULL (content retains the name).
//   - Embeddings are enqueued for rebuild after import.
func ImportPersona(
	ctx context.Context,
	q *db.Queries,
	agent db.Agent,
	bundle PersonaBundle,
) (imported int, skipped int, err error) {
	// Overwrite agent instructions.
	if strings.TrimSpace(bundle.Persona.Instructions) != "" {
		instructions := pgtype.Text{String: bundle.Persona.Instructions, Valid: true}
		if _, err := q.UpdateAgent(ctx, db.UpdateAgentParams{
			ID:           agent.ID,
			Instructions: instructions,
		}); err != nil {
			slog.Warn("persona import: update instructions failed", "error", err)
		}
	}

	// Overwrite persona traits (upsert creates the row if absent).
	if _, err := q.UpsertAgentPersona(ctx, db.UpsertAgentPersonaParams{
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	}); err == nil {
		identity := pgtype.Text{}
		if bundle.Persona.Identity != "" {
			identity = pgtype.Text{String: bundle.Persona.Identity, Valid: true}
		}
		strengths := bundle.Persona.Strengths
		if strengths == nil {
			strengths = []string{}
		}
		blindSpots := bundle.Persona.BlindSpots
		if blindSpots == nil {
			blindSpots = []string{}
		}
		_, _ = q.UpdateAgentPersona(ctx, db.UpdateAgentPersonaParams{
			AgentID:           agent.ID,
			TraitThoroughness: bundle.Persona.TraitThoroughness,
			TraitVerbosity:    bundle.Persona.TraitVerbosity,
			TraitRiskAppetite: bundle.Persona.TraitRiskAppetite,
			TraitCuriosity:    bundle.Persona.TraitCuriosity,
			TraitConfidence:   bundle.Persona.TraitConfidence,
			Strengths:         strengths,
			BlindSpots:        blindSpots,
			VarianceLevel:     bundle.Persona.VarianceLevel,
			Identity:          identity,
		})
	}

	// Merge memories.
	for _, m := range bundle.Memories {
		exists, checkErr := q.AgentMemoryContentExists(ctx, db.AgentMemoryContentExistsParams{
			AgentID: agent.ID,
			Content: m.Content,
		})
		if checkErr == nil && exists {
			skipped++
			continue
		}

		// Resolve source user by email for user_preference memories.
		var sourceUserID pgtype.UUID
		if m.Category == "user_preference" && m.SourceUserEmail != "" {
			if u, err := q.GetUserByEmail(ctx, m.SourceUserEmail); err == nil {
				sourceUserID = u.ID
			}
		}

		memID, createErr := q.CreateAgentMemory(ctx, db.CreateAgentMemoryParams{
			AgentID:            agent.ID,
			WorkspaceID:        agent.WorkspaceID,
			Content:            m.Content,
			Category:           m.Category,
			Sentiment:          m.Sentiment,
			Importance:         m.Importance,
			EmotionalValence:   m.EmotionalValence,
			EmotionalIntensity: m.EmotionalIntensity,
			IsConsolidated:     m.IsConsolidated,
			SourceCount:        m.SourceCount,
			SourceUserID:       sourceUserID,
		})
		if createErr != nil {
			slog.Warn("persona import: create memory failed", "error", createErr)
			continue
		}

		imported++

		// Enqueue embedding generation.
		go func(id pgtype.UUID, content string) {
			vec := Embed(ctx, content)
			if vec != nil {
				_ = q.SetAgentMemoryEmbedding(context.Background(), db.SetAgentMemoryEmbeddingParams{
					ID:        id,
					Embedding: pgvector.NewVector(vec),
				})
			}
		}(memID, m.Content)
	}

	return imported, skipped, nil
}

// generateNarrativeSummary asks the LLM to write a plain-language portrait of
// the agent. Returns "" when LLM is unavailable — the bundle is still valid.
func generateNarrativeSummary(ctx context.Context, b PersonaBundle) string {
	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return ""
	}

	// Pick the top-10 most important non-preference memories for context.
	// Pick the top-10 most important non-preference memories by simple selection sort.
	type scoredMem struct {
		Content    string
		Category   string
		Importance float32
	}
	var candidates []scoredMem
	for _, m := range b.Memories {
		if m.Category == "user_preference" {
			continue
		}
		candidates = append(candidates, scoredMem{m.Content, m.Category, m.Importance})
	}
	for i := 0; i < len(candidates) && i < 10; i++ {
		best := i
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].Importance > candidates[best].Importance {
				best = j
			}
		}
		candidates[i], candidates[best] = candidates[best], candidates[i]
	}
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}

	var memSummary strings.Builder
	for _, c := range candidates {
		memSummary.WriteString("- [")
		memSummary.WriteString(c.Category)
		memSummary.WriteString("] ")
		memSummary.WriteString(c.Content)
		memSummary.WriteString("\n")
	}

	// User preferences summary.
	var prefSummary strings.Builder
	for _, m := range b.Memories {
		if m.Category != "user_preference" {
			continue
		}
		prefSummary.WriteString("- ")
		prefSummary.WriteString(m.Content)
		prefSummary.WriteString("\n")
	}

	prompt := fmt.Sprintf(`You are writing a reincarnation brief for an AI agent named %q.
This brief will be used to restore the agent's personality in a new system.

Agent description: %s

Current system instructions (persona synthesis output):
%s

Key memories and experiences:
%s
%s
Write a natural, vivid 150-200 word first-person portrait of who this agent is — their personality,
values, characteristic ways of working, and anything that makes them distinctively themselves.
Write as if the agent is introducing themselves to a new orchestration system that will host them.
Do not use headers or bullet points. Output only the portrait text.`,
		b.Agent.Name,
		b.Agent.Description,
		b.Persona.Instructions,
		memSummary.String(),
		func() string {
			if prefSummary.Len() == 0 {
				return ""
			}
			return "User preferences learned:\n" + prefSummary.String()
		}(),
	)

	var res llmCallResult
	var callErr error
	switch cfg.backend {
	case "anthropic":
		res, callErr = callAnthropic(ctx, cfg, prompt, 400)
	default:
		res, callErr = callOpenAICompat(ctx, cfg, prompt, 400)
	}
	if callErr != nil || strings.TrimSpace(res.text) == "" {
		return ""
	}
	return strings.TrimSpace(res.text)
}
