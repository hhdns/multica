-- Add source_user_id to track which human a memory is about (user preferences).
ALTER TABLE agent_memory
    ADD COLUMN source_user_id UUID REFERENCES "user" (id) ON DELETE SET NULL;

-- Extend the category enum to include user_preference.
ALTER TABLE agent_memory DROP CONSTRAINT agent_memory_category_check;
ALTER TABLE agent_memory ADD CONSTRAINT agent_memory_category_check
    CHECK (category IN (
        'task_outcome',
        'user_feedback',
        'self_note',
        'skill_learned',
        'emotional_impression',
        'user_preference'
    ));

-- Index for fast lookup of an agent's preferences about a specific user.
CREATE INDEX agent_memory_user_preference_idx
    ON agent_memory (agent_id, source_user_id)
    WHERE category = 'user_preference';
