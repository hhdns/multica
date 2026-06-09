-- Add conversation_episode to the memory category enum and add
-- episode_recall_count to agent_persona for configurable temporal injection.

ALTER TABLE agent_memory DROP CONSTRAINT agent_memory_category_check;
ALTER TABLE agent_memory ADD CONSTRAINT agent_memory_category_check
    CHECK (category IN (
        'task_outcome',
        'user_feedback',
        'self_note',
        'skill_learned',
        'emotional_impression',
        'user_preference',
        'conversation_episode'
    ));

-- How many recent conversation_episode memories to inject into the task brief
-- via the temporal channel (independent of semantic search).
ALTER TABLE agent_persona
    ADD COLUMN episode_recall_count INT NOT NULL DEFAULT 5;

-- Fast lookup of recent episodes for temporal injection.
CREATE INDEX agent_memory_episode_recent_idx
    ON agent_memory (agent_id, created_at DESC)
    WHERE category = 'conversation_episode';
