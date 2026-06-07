-- name: CreateAgentMemory :one
INSERT INTO agent_memory (
    agent_id, workspace_id, content, category, sentiment,
    source_issue_id, source_task_id, importance
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: SetAgentMemoryEmbedding :exec
UPDATE agent_memory
SET embedding = $2
WHERE id = $1;

-- name: ListAgentMemories :many
SELECT * FROM agent_memory
WHERE agent_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: SearchAgentMemories :many
-- Semantic search: returns the top-k memories for an agent ordered by cosine
-- similarity to the query embedding. Rows without an embedding are excluded.
SELECT
    id,
    agent_id,
    workspace_id,
    content,
    category,
    sentiment,
    source_issue_id,
    source_task_id,
    embedding,
    importance,
    created_at,
    CAST(1 - (embedding <=> $2) AS float8) AS similarity
FROM agent_memory
WHERE agent_id = $1
  AND embedding IS NOT NULL
ORDER BY embedding <=> $2
LIMIT $3;

-- name: CountAgentMemories :one
SELECT COUNT(*) FROM agent_memory WHERE agent_id = $1;

-- name: DeleteOldAgentMemories :exec
-- Prune memories beyond the retention limit (keep the most recent N rows).
DELETE FROM agent_memory
WHERE agent_memory.id IN (
    SELECT m.id FROM agent_memory m
    WHERE m.agent_id = $1
    ORDER BY m.created_at DESC
    OFFSET $2
);
