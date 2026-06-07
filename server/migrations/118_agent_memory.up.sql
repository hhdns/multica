-- Enable pgvector extension (idempotent).
CREATE EXTENSION IF NOT EXISTS vector;

-- agent_memory stores episodic memories extracted from completed tasks.
-- Each row captures a single meaningful event (decision, outcome, feedback)
-- associated with a specific agent. The embedding column enables semantic
-- similarity search so the daemon can retrieve relevant past experiences
-- when starting a new task.
CREATE TABLE agent_memory (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        UUID        NOT NULL REFERENCES agent (id) ON DELETE CASCADE,
    workspace_id    UUID        NOT NULL REFERENCES workspace (id) ON DELETE CASCADE,

    -- The plain-text summary of what happened, used for display and as the
    -- source for embedding regeneration if the model changes.
    content         TEXT        NOT NULL,

    -- Memory category for coarse filtering without a vector scan.
    -- 'task_outcome'  — what happened when the agent worked on an issue
    -- 'user_feedback' — explicit praise / criticism from a human
    -- 'self_note'     — agent-generated reflection (future use)
    category        TEXT        NOT NULL DEFAULT 'task_outcome'
                    CHECK (category IN ('task_outcome', 'user_feedback', 'self_note')),

    -- Sentiment: 'positive', 'negative', 'neutral'.
    sentiment       TEXT        NOT NULL DEFAULT 'neutral'
                    CHECK (sentiment IN ('positive', 'negative', 'neutral')),

    -- Optional reference back to the issue and task this memory came from.
    source_issue_id UUID        REFERENCES issue (id) ON DELETE SET NULL,
    source_task_id  UUID        REFERENCES agent_task_queue (id) ON DELETE SET NULL,

    -- 1536-dim vector (text-embedding-3-small) or 768-dim (nomic-embed-text
    -- for Ollama). Nullable until the embedding service has processed the row.
    embedding       vector(1536),

    -- Soft importance weight (0.0–1.0). Defaults to 0.5; the signal system
    -- bumps it for strongly-weighted praise/criticism interactions.
    importance      REAL        NOT NULL DEFAULT 0.5
                    CHECK (importance BETWEEN 0.0 AND 1.0),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Fast lookup of all memories for a given agent (chronological paging).
CREATE INDEX agent_memory_agent_id_created_at_idx
    ON agent_memory (agent_id, created_at DESC);

-- pgvector approximate nearest-neighbour index.
-- HNSW is the recommended index type for query-time cosine similarity search.
-- Built only when embeddings are populated; empty-vector rows don't degrade it.
CREATE INDEX agent_memory_embedding_hnsw_idx
    ON agent_memory USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
