-- name: CreateChatSession :one
INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, runtime_id)
VALUES ($1, $2, $3, $4, (SELECT runtime_id FROM agent WHERE id = $2))
RETURNING *;

-- name: GetChatSession :one
SELECT * FROM chat_session
WHERE id = $1;

-- name: GetChatSessionInWorkspace :one
SELECT * FROM chat_session
WHERE id = $1 AND workspace_id = $2;

-- name: ListChatSessionsByCreator :many
-- Returns active sessions with a boolean unread flag. Unread is strictly
-- per-session: either the user has uncleared assistant replies in this
-- session or they don't. Counting messages would be misleading.
SELECT cs.*,
       (cs.unread_since IS NOT NULL)::bool AS has_unread
FROM chat_session cs
WHERE cs.workspace_id = $1 AND cs.creator_id = $2 AND cs.status = 'active'
ORDER BY cs.updated_at DESC;

-- name: ListAllChatSessionsByCreator :many
SELECT cs.*,
       (cs.unread_since IS NOT NULL)::bool AS has_unread
FROM chat_session cs
WHERE cs.workspace_id = $1 AND cs.creator_id = $2
ORDER BY cs.updated_at DESC;

-- name: UpdateChatSessionTitle :one
UPDATE chat_session SET title = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateChatSessionSession :exec
-- Updates the resume pointer for a chat session. Empty/NULL inputs are
-- ignored via COALESCE so a task that completes without a session_id (e.g.
-- the agent crashed before establishing one) cannot wipe out a previously
-- recorded resume pointer. This makes the chat memory robust against
-- intermittent agent failures.
UPDATE chat_session
SET session_id = COALESCE(sqlc.narg('session_id'), session_id),
    work_dir = COALESCE(sqlc.narg('work_dir'), work_dir),
    runtime_id = COALESCE(sqlc.narg('runtime_id'), runtime_id),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: LockChatSessionForDelete :one
-- Acquires an exclusive (FOR UPDATE) row lock on chat_session(id). Used by
-- the delete path so that a concurrent SendChatMessage cannot enqueue a new
-- agent_task_queue row referencing this session between our cancel and
-- delete steps. The FK from agent_task_queue.chat_session_id takes a
-- KEY SHARE lock on the parent row during INSERT validation, which
-- conflicts with FOR UPDATE — concurrent inserts block here and then fail
-- their FK check after we commit the delete.
SELECT id FROM chat_session
WHERE id = $1
FOR UPDATE;

-- name: DeleteChatSession :exec
-- Hard delete. chat_message rows cascade via FK ON DELETE CASCADE; the
-- chat_session_id on agent_task_queue is set NULL by FK so completed/failed
-- task history survives the session being removed. Callers MUST run inside
-- the same transaction that holds LockChatSessionForDelete and that has
-- already cancelled any in-flight tasks (see CancelAgentTasksByChatSession)
-- so the daemon does not keep running work whose result has nowhere to
-- land. workspace_id in the WHERE clause is a SQL-layer tenant guard; see
-- DeleteIssue.
DELETE FROM chat_session WHERE id = $1 AND workspace_id = $2;

-- name: TouchChatSession :exec
UPDATE chat_session SET updated_at = now()
WHERE id = $1;

-- name: CreateChatMessage :one
INSERT INTO chat_message (chat_session_id, role, content, task_id, failure_reason, elapsed_ms)
VALUES ($1, $2, $3, sqlc.narg(task_id), sqlc.narg(failure_reason), sqlc.narg(elapsed_ms))
RETURNING *;

-- name: LinkChatMessageToTask :exec
UPDATE chat_message
SET task_id = $2
WHERE id = $1 AND role = 'user';

-- name: DeleteUserChatMessageByTask :one
DELETE FROM chat_message
WHERE task_id = $1 AND role = 'user'
RETURNING *;

-- name: ListChatMessages :many
SELECT * FROM chat_message
WHERE chat_session_id = $1
ORDER BY created_at ASC;

-- name: ListChatMessagesPage :many
SELECT * FROM chat_message
WHERE chat_session_id = $1
  AND (
    sqlc.narg('before_created_at')::timestamptz IS NULL
    OR (created_at, id) < (sqlc.narg('before_created_at')::timestamptz, sqlc.narg('before_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: GetChatMessage :one
SELECT * FROM chat_message
WHERE id = $1;

-- name: CreateChatTask :one
INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, chat_session_id, initiator_user_id)
VALUES ($1, $2, NULL, 'queued', $3, $4, $5)
RETURNING *;

-- name: GetLastChatTaskSession :one
-- Returns the most recent task in this chat session that managed to record a
-- session_id. Includes both completed and failed tasks: even a failed task
-- may have established a real agent session before failing, and we'd rather
-- resume there than start over and lose conversation memory. Used as a
-- fallback when chat_session.session_id is NULL. Resume-unsafe failures are
-- excluded because replaying those sessions deterministically reproduces the
-- same terminal state.
SELECT session_id, work_dir, runtime_id FROM agent_task_queue
WHERE chat_session_id = $1
  AND (
    status = 'completed'
    OR (
      status = 'failed'
      AND COALESCE(failure_reason, '') NOT IN ('iteration_limit', 'agent_fallback_message', 'api_invalid_request', 'codex_semantic_inactivity')
      AND NOT (COALESCE(error, '') ILIKE '%400%' AND COALESCE(error, '') ILIKE '%invalid_request_error%')
    )
  )
  AND session_id IS NOT NULL
ORDER BY completed_at DESC
LIMIT 1;

-- name: GetPendingChatTask :one
-- Returns the most recent in-flight task for a chat session, if any.
-- Used by the frontend to recover pending state after refresh / reopen.
-- created_at is the anchor for the chat StatusPill timer (it computes
-- elapsed = now - task.created_at), so the pill survives refresh / reopen
-- without "resetting to 0s".
SELECT id, agent_id, status, created_at FROM agent_task_queue
WHERE chat_session_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY created_at DESC
LIMIT 1;

-- name: ListPendingChatTasksForSession :many
-- Returns all in-flight tasks for a chat session. Used by group sessions
-- where multiple agents may be thinking simultaneously.
SELECT id, agent_id, status, created_at FROM agent_task_queue
WHERE chat_session_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY created_at ASC;

-- name: ListPendingChatTasksByCreator :many
-- Aggregate view of all in-flight chat tasks owned by a given creator in a
-- workspace. Drives the FAB's "running" indicator when the chat window is
-- closed and no single session's query is active.
SELECT atq.id AS task_id, atq.status, atq.chat_session_id
FROM agent_task_queue atq
JOIN chat_session cs ON cs.id = atq.chat_session_id
WHERE cs.workspace_id = $1
  AND cs.creator_id = $2
  AND atq.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY atq.created_at DESC;

-- name: MarkChatSessionRead :exec
-- Clears unread_since, dropping the session's unread count to 0.
UPDATE chat_session SET unread_since = NULL
WHERE id = $1;

-- name: SetUnreadSinceIfNull :exec
-- Atomically stamps the first unread assistant message's arrival time.
-- No-op if the session is already in "has unread" state — keeps the earliest
-- unread boundary stable across multiple incoming replies.
UPDATE chat_session SET unread_since = now()
WHERE id = $1 AND unread_since IS NULL;

-- name: GetMostRecentUserChatMessage :one
-- Returns the most recent role='user' message in a session. Used by the
-- Lark `/issue` command parser: when the user types `/issue` with no
-- title, the spec falls back to "use the previous user message as the
-- title". Bot replies (role='assistant') are excluded — only human
-- input qualifies as a fallback title source.
SELECT * FROM chat_message
WHERE chat_session_id = $1 AND role = 'user'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetRecentAgentChatMessages :many
-- Fetches the most recent visible chat messages across all sessions for an
-- agent, newest-first. Used for Layer-1 temporal injection at claim time:
-- raw verbatim messages give the agent working memory of recent exchanges
-- without requiring an LLM call.
SELECT cm.id, cm.chat_session_id, cm.role, cm.content, cm.created_at
FROM chat_message cm
JOIN chat_session cs ON cs.id = cm.chat_session_id
WHERE cs.agent_id = @agent_id
  AND cs.workspace_id = @workspace_id
  AND cm.role IN ('user', 'assistant')
  AND cm.failure_reason IS NULL
  AND cm.content != ''
ORDER BY cm.created_at DESC
LIMIT @msg_limit;

-- name: MarkChatSessionEpisode :exec
-- Records the time at which a conversation_episode memory was generated
-- for this session. Used by arc-boundary detection to avoid duplicates.
UPDATE chat_session SET last_episode_at = now() WHERE id = $1;

-- name: GetChatSessionEpisodeState :one
-- Returns the fields needed to decide whether to generate an episode at
-- claim time: the session's last-activity and last-episode timestamps.
SELECT last_episode_at, updated_at FROM chat_session WHERE id = $1;

-- name: CreateGroupChatSession :one
INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, runtime_id, is_group, routing_mode)
VALUES ($1, $2, $3, $4, (SELECT runtime_id FROM agent WHERE id = $2), true, $5)
RETURNING *;

-- name: AddChatSessionParticipants :exec
INSERT INTO chat_session_participant (chat_session_id, agent_id)
SELECT @chat_session_id, unnest(@agent_ids::uuid[])
ON CONFLICT DO NOTHING;

-- name: GetChatSessionParticipants :many
SELECT agent_id FROM chat_session_participant
WHERE chat_session_id = $1
ORDER BY joined_at ASC;

-- name: GetLastAssistantAgentInSession :one
-- Used by relay routing: find the agent_id of the most recent assistant message.
SELECT agent_id FROM chat_message
WHERE chat_session_id = $1
  AND role = 'assistant'
  AND agent_id IS NOT NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateChatMessageWithAgent :one
INSERT INTO chat_message (chat_session_id, role, content, task_id, agent_id, failure_reason, elapsed_ms)
VALUES ($1, $2, $3, sqlc.narg(task_id), sqlc.narg(agent_id), sqlc.narg(failure_reason), sqlc.narg(elapsed_ms))
RETURNING *;
