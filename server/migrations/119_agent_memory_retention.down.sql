ALTER TABLE agent_memory DROP CONSTRAINT agent_memory_category_check;
ALTER TABLE agent_memory ADD CONSTRAINT agent_memory_category_check
    CHECK (category IN ('task_outcome', 'user_feedback', 'self_note'));

ALTER TABLE agent_memory
    DROP COLUMN emotional_valence,
    DROP COLUMN emotional_intensity,
    DROP COLUMN access_count,
    DROP COLUMN last_accessed_at,
    DROP COLUMN is_consolidated,
    DROP COLUMN source_count;
