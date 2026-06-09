ALTER TABLE chat_message DROP COLUMN IF EXISTS agent_id;
DROP TABLE IF EXISTS chat_session_participant;
ALTER TABLE chat_session DROP COLUMN IF EXISTS routing_mode;
ALTER TABLE chat_session DROP COLUMN IF EXISTS is_group;
