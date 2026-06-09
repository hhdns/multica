DROP INDEX IF EXISTS agent_memory_user_preference_idx;

ALTER TABLE agent_memory DROP CONSTRAINT agent_memory_category_check;
ALTER TABLE agent_memory ADD CONSTRAINT agent_memory_category_check
    CHECK (category IN (
        'task_outcome',
        'user_feedback',
        'self_note',
        'skill_learned',
        'emotional_impression'
    ));

ALTER TABLE agent_memory DROP COLUMN source_user_id;
