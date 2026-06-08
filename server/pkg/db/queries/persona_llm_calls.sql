-- name: InsertPersonaLLMCall :exec
INSERT INTO persona_llm_calls (agent_id, workspace_id, call_type, backend, model, input_tokens, output_tokens, latency_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListPersonaLLMCalls :many
SELECT id, agent_id, workspace_id, call_type, backend, model, input_tokens, output_tokens, latency_ms, created_at
FROM persona_llm_calls
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListAgentLLMCalls :many
SELECT id, agent_id, workspace_id, call_type, backend, model, input_tokens, output_tokens, latency_ms, created_at
FROM persona_llm_calls
WHERE agent_id = $1
ORDER BY created_at DESC
LIMIT $2;
