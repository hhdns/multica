-- name: GetAgentPersona :one
SELECT * FROM agent_persona
WHERE agent_id = $1;

-- name: UpsertAgentPersona :one
INSERT INTO agent_persona (agent_id, workspace_id)
VALUES ($1, $2)
ON CONFLICT (agent_id) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: UpdateAgentPersona :one
UPDATE agent_persona SET
    trait_thoroughness  = $2,
    trait_verbosity     = $3,
    trait_risk_appetite = $4,
    trait_curiosity     = $5,
    trait_confidence    = $6,
    strengths           = $7,
    blind_spots         = $8,
    variance_level      = $9,
    identity            = $10,
    updated_at          = now()
WHERE agent_id = $1
RETURNING *;

-- name: UpdateAgentPersonaMood :one
UPDATE agent_persona SET
    mood            = $2,
    mood_updated_at = now(),
    updated_at      = now()
WHERE agent_id = $1
RETURNING *;

-- name: IncrementAgentPersonaSignalCount :exec
UPDATE agent_persona SET
    signal_count = signal_count + 1,
    updated_at   = now()
WHERE agent_id = $1;

-- name: CreateAgentInteractionSignal :one
INSERT INTO agent_interaction_signal (
    agent_id, workspace_id, type, content, weight,
    source_type, source_id, source_user_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListAgentInteractionSignals :many
SELECT * FROM agent_interaction_signal
WHERE agent_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListUnprocessedAgentSignals :many
SELECT * FROM agent_interaction_signal
WHERE agent_id = $1 AND NOT processed
ORDER BY created_at ASC;

-- name: MarkAgentSignalsProcessed :exec
UPDATE agent_interaction_signal
SET processed = true
WHERE agent_id = $1 AND NOT processed;

-- name: SetAgentPersonaSynthesizedAt :exec
UPDATE agent_persona SET
    last_synthesized_at = now(),
    updated_at          = now()
WHERE agent_id = $1;

-- name: DriftAgentPersonaTraits :one
UPDATE agent_persona SET
    trait_thoroughness  = GREATEST(0, LEAST(100, trait_thoroughness  + $2)),
    trait_verbosity     = GREATEST(0, LEAST(100, trait_verbosity     + $3)),
    trait_risk_appetite = GREATEST(0, LEAST(100, trait_risk_appetite + $4)),
    trait_curiosity     = GREATEST(0, LEAST(100, trait_curiosity     + $5)),
    trait_confidence    = GREATEST(0, LEAST(100, trait_confidence    + $6)),
    updated_at          = now()
WHERE agent_id = $1
RETURNING *;

-- name: CountRecentAgentTaskOutcomes :one
SELECT
    COUNT(*) FILTER (WHERE status = 'completed') AS completed_count,
    COUNT(*) FILTER (WHERE status = 'failed')    AS failed_count
FROM agent_task_queue
WHERE agent_id = $1
  AND status IN ('completed', 'failed')
  AND completed_at >= now() - interval '7 days'
LIMIT 1;
