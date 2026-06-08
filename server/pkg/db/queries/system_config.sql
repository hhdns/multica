-- name: GetSystemConfig :one
SELECT value FROM system_config WHERE key = $1;

-- name: UpsertSystemConfig :exec
INSERT INTO system_config (key, value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();
