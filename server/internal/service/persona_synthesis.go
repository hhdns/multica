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
	synthesisMaxTok = 1024

	// How many drift batches (each = driftBatchThreshold signals) must pass
	// before auto-synthesis kicks in. 3 batches × 5 signals = 15 signals.
	synthesisAfterBatches = 3

	anthropicMessagesPath = "/v1/messages"
	openAIChatPath        = "/v1/chat/completions"
)

// synthesisConfig is resolved once per call from environment variables.
type synthesisConfig struct {
	// backend is "anthropic", "openai", or "" (disabled).
	backend  string
	endpoint string
	apiKey   string
	model    string
}

// ResolveSynthesisBackend returns the active LLM backend name for persona
// synthesis: "anthropic", "openai" (OpenAI-compat), or "" (disabled).
// Safe to expose to the frontend — never includes the actual API key.
func ResolveSynthesisBackend() string {
	return resolveSynthesisConfig().backend
}

// resolveSynthesisConfig reads environment variables and returns the active
// synthesis configuration.
//
// Resolution order:
//  1. PERSONA_SYNTHESIS_ENABLED=false → disabled
//  2. PERSONA_SYNTHESIS_BASE_URL set → openai-compat backend
//  3. ANTHROPIC_API_KEY set → anthropic backend
//  4. Neither → disabled
func resolveSynthesisConfig() synthesisConfig {
	if strings.EqualFold(os.Getenv("PERSONA_SYNTHESIS_ENABLED"), "false") {
		return synthesisConfig{}
	}

	// OpenAI-compat backend (covers Ollama, vLLM, LM Studio, Azure, etc.)
	if base := os.Getenv("PERSONA_SYNTHESIS_BASE_URL"); base != "" {
		apiKey := os.Getenv("PERSONA_SYNTHESIS_API_KEY")
		model := os.Getenv("PERSONA_SYNTHESIS_MODEL")
		if model == "" {
			model = "gpt-4o-mini" // sensible fallback for generic compat endpoints
		}
		return synthesisConfig{
			backend:  "openai",
			endpoint: strings.TrimRight(base, "/") + openAIChatPath,
			apiKey:   apiKey,
			model:    model,
		}
	}

	// Anthropic direct backend
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		model := os.Getenv("PERSONA_SYNTHESIS_MODEL")
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return synthesisConfig{
			backend:  "anthropic",
			endpoint: "https://api.anthropic.com" + anthropicMessagesPath,
			apiKey:   key,
			model:    model,
		}
	}

	return synthesisConfig{} // disabled
}

// llmCallResult holds the output of a single LLM API call.
type llmCallResult struct {
	text         string
	inputTokens  int32
	outputTokens int32
	latencyMs    int32
}

// logPersonaLLMCall records a completed LLM call to the append-only audit table.
// Errors are logged at debug level and never propagate — this is best-effort.
func logPersonaLLMCall(
	ctx context.Context,
	q *db.Queries,
	callType string,
	agentID, workspaceID pgtype.UUID,
	backend, model string,
	res llmCallResult,
) {
	if err := q.InsertPersonaLLMCall(ctx, db.InsertPersonaLLMCallParams{
		AgentID:      agentID,
		WorkspaceID:  workspaceID,
		CallType:     callType,
		Backend:      backend,
		Model:        model,
		InputTokens:  res.inputTokens,
		OutputTokens: res.outputTokens,
		LatencyMs:    res.latencyMs,
	}); err != nil {
		slog.Debug("persona: log llm call failed", "error", err, "call_type", callType)
	}
}

// SynthesizeAgentInstructions calls the configured LLM backend to generate
// new instructions for the agent based on its current persona data, then
// persists the result to agent.instructions and marks last_synthesized_at.
func SynthesizeAgentInstructions(
	ctx context.Context,
	q *db.Queries,
	agentID, workspaceID pgtype.UUID,
	agentName string,
	currentInstructions string,
) error {
	cfg := resolveSynthesisConfig()
	if cfg.backend == "" {
		return fmt.Errorf("persona synthesis is disabled (set ANTHROPIC_API_KEY or PERSONA_SYNTHESIS_BASE_URL)")
	}

	persona, err := q.GetAgentPersona(ctx, agentID)
	if err != nil {
		return fmt.Errorf("get agent persona: %w", err)
	}

	prompt := buildSynthesisPrompt(agentName, currentInstructions, persona)

	var res llmCallResult
	switch cfg.backend {
	case "anthropic":
		res, err = callAnthropic(ctx, cfg, prompt, synthesisMaxTok)
	case "openai":
		res, err = callOpenAICompat(ctx, cfg, prompt, synthesisMaxTok)
	default:
		return fmt.Errorf("unknown synthesis backend: %s", cfg.backend)
	}
	if err != nil {
		return fmt.Errorf("synthesis call (%s): %w", cfg.backend, err)
	}
	logPersonaLLMCall(ctx, q, "synthesis", agentID, workspaceID, cfg.backend, cfg.model, res)
	instructions := res.text

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
		slog.Warn("persona: set synthesized_at failed", "error", err2, "agent_id", agentID)
	}

	return nil
}

// MaybeSynthesizeAfterDrift triggers auto-synthesis after every
// synthesisAfterBatches drift runs. Called from MaybeApplyTraitDrift.
func MaybeSynthesizeAfterDrift(
	ctx context.Context,
	q *db.Queries,
	agentID pgtype.UUID,
) {
	if resolveSynthesisConfig().backend == "" {
		return // synthesis not configured — skip silently
	}

	persona, err := q.GetAgentPersona(ctx, agentID)
	if err != nil {
		return
	}

	batchCount := int(persona.SignalCount) / driftBatchThreshold
	if batchCount == 0 || batchCount%synthesisAfterBatches != 0 {
		return
	}

	agent, err := q.GetAgent(ctx, agentID)
	if err != nil {
		slog.Warn("persona: get agent for synthesis failed", "error", err, "agent_id", agentID)
		return
	}

	if err := SynthesizeAgentInstructions(
		ctx, q, agentID, agent.WorkspaceID,
		agent.Name,
		agent.Instructions,
	); err != nil {
		slog.Warn("persona: auto-synthesis failed", "error", err, "agent_id", agentID)
	}
}

// buildSynthesisPrompt builds the user-turn message sent to the LLM.
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

// ---- Anthropic backend ----

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
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
	Usage struct {
		InputTokens  int32 `json:"input_tokens"`
		OutputTokens int32 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func callAnthropic(ctx context.Context, cfg synthesisConfig, userPrompt string, maxTok int) (llmCallResult, error) {
	body, err := json.Marshal(anthropicRequest{
		Model:     cfg.model,
		MaxTokens: maxTok,
		System:    "You are a concise technical writer. Output only the requested text.",
		Messages:  []anthropicMessage{{Role: "user", Content: userPrompt}},
	})
	if err != nil {
		return llmCallResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return llmCallResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	latencyMs := int32(time.Since(start).Milliseconds())
	if err != nil {
		return llmCallResult{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return llmCallResult{}, err
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return llmCallResult{}, fmt.Errorf("unmarshal response: %w (status %d)", err, resp.StatusCode)
	}
	if ar.Error != nil {
		return llmCallResult{}, fmt.Errorf("api error %s: %s", ar.Error.Type, ar.Error.Message)
	}
	for _, block := range ar.Content {
		if block.Type == "text" && block.Text != "" {
			return llmCallResult{
				text:         block.Text,
				inputTokens:  ar.Usage.InputTokens,
				outputTokens: ar.Usage.OutputTokens,
				latencyMs:    latencyMs,
			}, nil
		}
	}
	return llmCallResult{}, fmt.Errorf("no text block in response (status %d)", resp.StatusCode)
}

// ---- OpenAI-compat backend ----

type openAIRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []openAIMessage `json:"messages"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int32 `json:"prompt_tokens"`
		CompletionTokens int32 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func callOpenAICompat(ctx context.Context, cfg synthesisConfig, userPrompt string, maxTok int) (llmCallResult, error) {
	body, err := json.Marshal(openAIRequest{
		Model:     cfg.model,
		MaxTokens: maxTok,
		Messages: []openAIMessage{
			{Role: "system", Content: "You are a concise technical writer. Output only the requested text."},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return llmCallResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return llmCallResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	}

	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	latencyMs := int32(time.Since(start).Milliseconds())
	if err != nil {
		return llmCallResult{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return llmCallResult{}, err
	}

	var or openAIResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return llmCallResult{}, fmt.Errorf("unmarshal response: %w (status %d)", err, resp.StatusCode)
	}
	if or.Error != nil {
		return llmCallResult{}, fmt.Errorf("api error: %s", or.Error.Message)
	}
	if len(or.Choices) > 0 {
		return llmCallResult{
			text:         or.Choices[0].Message.Content,
			inputTokens:  or.Usage.PromptTokens,
			outputTokens: or.Usage.CompletionTokens,
			latencyMs:    latencyMs,
		}, nil
	}
	return llmCallResult{}, fmt.Errorf("no choices in response (status %d)", resp.StatusCode)
}
