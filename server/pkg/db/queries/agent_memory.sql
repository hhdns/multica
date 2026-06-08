-- name: CreateAgentMemory :one
INSERT INTO agent_memory (
    agent_id, workspace_id, content, category, sentiment,
    source_issue_id, source_task_id, importance,
    emotional_valence, emotional_intensity,
    is_consolidated, source_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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

-- name: ListEmbeddedMemories :many
-- Returns all memories with embeddings for a given agent, used by the
-- compaction pass to find clusters of similar episodes.
SELECT id, content, category, sentiment, importance,
       emotional_valence, emotional_intensity, embedding, created_at
FROM agent_memory
WHERE agent_id = $1
  AND embedding IS NOT NULL
  AND is_consolidated = false
  AND category IN ('task_outcome', 'user_feedback')
ORDER BY created_at DESC
LIMIT $2;

-- name: DeleteMemoriesByIDs :exec
-- Bulk-delete memories by ID, used after compaction to remove merged episodes.
DELETE FROM agent_memory WHERE id = ANY($1::uuid[]);

-- name: ListMemoriesForIssue :many
-- Returns recent memories for a specific agent + issue pair, newest first.
-- Used to detect re-trigger scenarios (previously failed = higher importance).
SELECT id, sentiment, created_at FROM agent_memory
WHERE agent_id = $1 AND source_issue_id = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: DeleteOldAgentMemories :exec
-- Tiered retention pruning: each category group has its own cap so high-value
-- emotional and skill memories cannot be crowded out by routine task episodes.
--
-- Tier limits (kept in sync with constants in service/agent_memory.go):
--   emotional_impression : 20
--   skill_learned        : 30
--   everything else      : 150   (task_outcome, user_feedback, self_note)
--
-- Within each tier, the multi-factor retention score ranks memories:
--   35% importance · 25% recency (45-day half-life) · 20% access frequency
--   15% emotional intensity · 5% consolidated bonus
DELETE FROM agent_memory AS target
WHERE target.agent_id = $1
  AND target.id IN (
    SELECT r.id
    FROM (
        SELECT am.id,
               ROW_NUMBER() OVER (
                   PARTITION BY
                       CASE am.category
                           WHEN 'emotional_impression' THEN 'emotional'
                           WHEN 'skill_learned'        THEN 'skill'
                           ELSE                             'episodic'
                       END
                   ORDER BY (
                         0.35 * am.importance
                       + 0.25 * exp(-EXTRACT(EPOCH FROM (NOW() - am.created_at)) / (45.0 * 86400))
                       + 0.20 * ln(1.0 + am.access_count::float8)
                       + 0.15 * am.emotional_intensity
                       + 0.05 * CASE WHEN am.is_consolidated THEN 1.0 ELSE 0.0 END
                   ) DESC
               ) AS rn,
               CASE am.category
                   WHEN 'emotional_impression' THEN 20
                   WHEN 'skill_learned'        THEN 30
                   ELSE                             150
               END AS cat_limit
        FROM agent_memory am
        WHERE am.agent_id = $1
    ) r
    WHERE r.rn > r.cat_limit
);
