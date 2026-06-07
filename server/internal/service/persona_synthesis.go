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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	anthropicAPIURL = "https://api.anthropic.com/v1/messages"
	synthesisModel  = "claude-haiku-4-5-20251001"
	synthesisMaxTok = 1024

	// How many drift batches (each = driftBatchThreshold signals) must pass
	// before auto-synthesis kicks in. 3 batches × 5 signals = 15 signals.
	synthesisAfterBatches = 3
)

// anthropicRequest is the minimal request shape for the Messages API.
type anthropicRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	System    string              `json:"system"`
	Messages  []anthropicMessage  `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// SynthesizeAgentInstructions calls Claude Haiku to generate new instructions
// for the agent based on its current persona data, then persists the result
// to agent.instructions and marks agent_persona.last_synthesized_at.
//
// The caller is responsible for supplying the agent's name and current
// instructions so we avoid an extra DB round-trip when called from a handler
// that already has this data. Pass empty strings when unavailable.
func SynthesizeAgentInstructions(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
	agentName string,
	currentInstructions string,
) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	persona, err := q.GetAgentPersona(ctx, agentID)
	if err != nil {
		return fmt.Errorf("get agent persona: %w", err)
	}

	prompt := buildSynthesisPrompt(agentName, currentInstructions, persona)

	instructions, err := callClaude(ctx, apiKey, prompt)
	if err != nil {
		return fmt.Errorf("claude synthesis call: %w", err)
	}
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return fmt.Errorf("synthesis returned empty instructions")
	}

	_, err = q.UpdateAgent(ctx, db.UpdateAgentParams{
		ID:           agentID,
		Instructions: pgtype.Text{String: instructions, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("update agent instructions: %w", err)
	}

	if err2 := q.SetAgentPersonaSynthesizedAt(ctx, agentID); err2 != nil {
		err = err2
		slog.Warn("persona: set synthesized_at failed", "error", err, "agent_id", agentID)
	}

	return nil
}

// MaybeSynthesizeAfterDrift triggers auto-synthesis after every
// synthesisAfterBatches drift runs. Called from MaybeApplyTraitDrift.
// Runs synchronously so the caller may want to wrap it in a goroutine.
func MaybeSynthesizeAfterDrift(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
) {
	persona, err := q.GetAgentPersona(ctx, agentID)
	if err != nil {
		return
	}

	// signal_count increments before drift; after a drift batch the count is a
	// multiple of driftBatchThreshold. Trigger every synthesisAfterBatches-th batch.
	batchCount := int(persona.SignalCount) / driftBatchThreshold
	if batchCount == 0 || batchCount%synthesisAfterBatches != 0 {
		return
	}

	// Fetch agent name + current instructions for the prompt.
	agent, err := q.GetAgent(ctx, agentID)
	if err != nil {
		slog.Warn("persona: get agent for synthesis failed", "error", err, "agent_id", agentID)
		return
	}

	if err := SynthesizeAgentInstructions(
		ctx, q, agentID,
		agent.Name,
		agent.Instructions,
	); err != nil {
		slog.Warn("persona: auto-synthesis failed", "error", err, "agent_id", agentID)
	}
}

// buildSynthesisPrompt builds the user-turn message sent to Claude.
func buildSynthesisPrompt(name, currentInstructions string, p db.AgentPersona) string {
	if name == "" {
		name = "this agent"
	}

	strengthsList := "none defined"
	if len(p.Strengths) > 0 {
		strengthsList = strings.Join(p.Strengths, ", ")
	}
	blindSpotsList := "none defined"
	if len(p.BlindSpots) > 0 {
		blindSpotsList = strings.Join(p.BlindSpots, ", ")
	}

	identity := p.Identity.String
	if !p.Identity.Valid || identity == "" {
		identity = "(not set)"
	}

	previous := currentInstructions
	if previous == "" {
		previous = "(none)"
	}

	return fmt.Sprintf(`You are rewriting the working instructions for an AI agent named "%s" based on its evolved personality data.

Current personality traits (each 0–100):
- Thoroughness: %d  (higher = more careful and thorough)
- Verbosity:    %d  (higher = more detailed responses)
- Risk appetite:%d  (higher = bolder, less conservative)
- Curiosity:    %d  (higher = more exploratory)
- Confidence:   %d  (higher = more assertive)

Current mood: %s
Spontaneity level: %d/100

Strengths: %s
Blind spots: %s
Self-identity: %s

Previous instructions (for reference, do NOT repeat verbatim):
%s

Write NEW concise instructions for this agent that reflect its personality.
The instructions should naturally express the trait scores above without
mentioning the numbers. Keep it under 300 words. Output only the instructions
text, no preamble, no explanation.`,
		name,
		p.TraitThoroughness,
		p.TraitVerbosity,
		p.TraitRiskAppetite,
		p.TraitCuriosity,
		p.TraitConfidence,
		p.Mood,
		p.VarianceLevel,
		strengthsList,
		blindSpotsList,
		identity,
		previous,
	)
}

// callClaude sends one user message to the Anthropic Messages API and returns
// the first text block of the response.
func callClaude(ctx context.Context, apiKey, userPrompt string) (string, error) {
	body, err := json.Marshal(anthropicRequest{
		Model:     synthesisModel,
		MaxTokens: synthesisMaxTok,
		System:    "You are a concise technical writer. Output only the requested text.",
		Messages: []anthropicMessage{
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return "", fmt.Errorf("unmarshal response: %w (status %d)", err, resp.StatusCode)
	}
	if ar.Error != nil {
		return "", fmt.Errorf("api error %s: %s", ar.Error.Type, ar.Error.Message)
	}
	for _, block := range ar.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text block in response (status %d)", resp.StatusCode)
}
