-- Group chat support: allow multiple agents to participate in a single chat session.

-- Mark a session as a group chat and record its routing mode.
-- routing_mode = 'mention': only @mentioned agents respond; no @ = silence.
-- routing_mode = 'relay':   @mention wins; otherwise the last-responding agent continues.
-- NULL routing_mode means single-agent session (backward compatible).
ALTER TABLE chat_session
    ADD COLUMN is_group     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN routing_mode TEXT CHECK (routing_mode IN ('mention', 'relay'));

-- Participant roster for group chats.
-- Single-agent sessions do not write rows here; agent_id on chat_session
-- remains the sole source of truth for those.
CREATE TABLE chat_session_participant (
    chat_session_id UUID NOT NULL REFERENCES chat_session(id) ON DELETE CASCADE,
    agent_id        UUID NOT NULL REFERENCES agent(id)        ON DELETE CASCADE,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_session_id, agent_id)
);

CREATE INDEX idx_csp_agent_id ON chat_session_participant(agent_id);

-- Record which agent produced each assistant message so the UI can show per-agent
-- attribution in group chats. NULL for legacy rows and single-agent sessions.
ALTER TABLE chat_message
    ADD COLUMN agent_id UUID REFERENCES agent(id) ON DELETE SET NULL;
