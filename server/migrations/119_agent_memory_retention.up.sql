-- Add emotional dimensions, access tracking, and consolidation marker to
-- agent_memory to support human-like retention scoring and memory compaction.

ALTER TABLE agent_memory
    -- Emotional valence: -1.0 (frustration/failure) → 1.0 (satisfaction/pride).
    ADD COLUMN emotional_valence   REAL    NOT NULL DEFAULT 0.0
        CHECK (emotional_valence   BETWEEN -1.0 AND  1.0),
    -- Emotional intensity: how vividly the agent "experienced" this event.
    -- High-intensity memories are harder to forget (mirrors human psychology).
    ADD COLUMN emotional_intensity REAL    NOT NULL DEFAULT 0.0
        CHECK (emotional_intensity BETWEEN  0.0 AND  1.0),
    -- Incremented each time this memory surfaces in a semantic search hit.
    -- Frequently-recalled memories are implicitly more valuable.
    ADD COLUMN access_count        INT     NOT NULL DEFAULT 0,
    ADD COLUMN last_accessed_at    TIMESTAMPTZ,
    -- Set true when this row was synthesised from multiple episodic memories
    -- during a compaction pass (Phase 5). Consolidated memories get a small
    -- retention bonus so hard-won lessons outlast their source episodes.
    ADD COLUMN is_consolidated     BOOL    NOT NULL DEFAULT false,
    ADD COLUMN source_count        INT     NOT NULL DEFAULT 1;

-- Expand the category set to include compacted memory types introduced in
-- the memory-improvement roadmap (skill_learned, emotional_impression).
ALTER TABLE agent_memory DROP CONSTRAINT agent_memory_category_check;
ALTER TABLE agent_memory ADD CONSTRAINT agent_memory_category_check
    CHECK (category IN (
        'task_outcome',
        'user_feedback',
        'self_note',
        'skill_learned',
        'emotional_impression'
    ));
