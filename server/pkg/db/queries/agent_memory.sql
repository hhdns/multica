-- name: CreateAgentMemory :one
INSERT INTO agent_memory (
    agent_id, workspace_id, content, category, sentiment,
    source_issue_id, source_task_id, importance,
    emotional_valence, emotional_intensity
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: SetAgentMemoryEmbedding :exec
UPDATE agent_memory
SET embedding = $2
WHERE id = $1;

-- name: BumpMemoryAccess :exec
-- Called after a semantic search so frequently-recalled memories score higher
-- on retention and are less likely to be pruned.
UPDATE agent_memory
SET access_count     = access_count + 1,
    last_accessed_at = NOW()
WHERE id = ANY($1::uuid[]);

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
    emotional_valence,
    emotional_intensity,
    access_count,
    last_accessed_at,
    is_consolidated,
    source_count,
    created_at,
    CAST(1 - (embedding <=> $2) AS float8) AS similarity
FROM agent_memory
WHERE agent_id = $1
  AND embedding IS NOT NULL
ORDER BY embedding <=> $2
LIMIT $3;

-- name: CountAgentMemories :one
SELECT COUNT(*) FROM agent_memory WHERE agent_id = $1;

-- name: ListMemoriesForIssue :many
-- Returns recent memories for a specific agent + issue pair, newest first.
-- Used to detect re-trigger scenarios (previously failed = higher importance).
SELECT id, sentiment, created_at FROM agent_memory
WHERE agent_id = $1 AND source_issue_id = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: DeleteOldAgentMemories :exec
-- Prune memories beyond the retention limit using a multi-factor retention
-- score rather than pure age. Keeps the memories most worth remembering:
--   35% importance   — explicitly weighted at record time
--   25% recency      — exponential decay with ~45-day half-life
--   20% access freq  — memories that surface in searches matter more
--   15% emotional    — vivid experiences are harder to forget
--    5% consolidated — compacted lessons get a small bonus
DELETE FROM agent_memory
WHERE agent_memory.id IN (
    SELECT m.id FROM agent_memory m
    WHERE m.agent_id = $1
    ORDER BY (
          0.35 * m.importance
        + 0.25 * exp(-EXTRACT(EPOCH FROM (NOW() - m.created_at)) / (45.0 * 86400))
        + 0.20 * ln(1.0 + m.access_count::float8)
        + 0.15 * m.emotional_intensity
        + 0.05 * CASE WHEN m.is_consolidated THEN 1.0 ELSE 0.0 END
    ) DESC
    OFFSET $2
);
