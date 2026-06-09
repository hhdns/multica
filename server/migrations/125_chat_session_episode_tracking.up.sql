-- Track when a conversation_episode memory was last generated for each
-- chat session. Used at claim time to detect arc boundaries and avoid
-- generating duplicate episode summaries.
ALTER TABLE chat_session ADD COLUMN last_episode_at TIMESTAMPTZ;
