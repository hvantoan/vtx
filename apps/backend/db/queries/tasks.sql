-- name: GetTask :one
SELECT id, title, status, prompt, workspace_path, created_at, updated_at
FROM tasks
WHERE id = $1 LIMIT 1;

-- name: ListTasks :many
SELECT id, title, status, prompt, workspace_path, created_at, updated_at
FROM tasks
ORDER BY created_at DESC;

-- name: CreateTask :one
INSERT INTO tasks (
    id, title, status, prompt, workspace_path
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING id, title, status, prompt, workspace_path, created_at, updated_at;

-- name: UpdateTaskStatus :exec
UPDATE tasks
SET status = $2, updated_at = CURRENT_TIMESTAMP
WHERE id = $1;

-- name: CreateTaskEvent :one
INSERT INTO task_events (
    task_id, seq, event_type, payload
) VALUES (
    $1, $2, $3, $4
)
RETURNING id, task_id, seq, event_type, payload, created_at;

-- name: ListTaskEvents :many
SELECT id, task_id, seq, event_type, payload, created_at
FROM task_events
WHERE task_id = $1
ORDER BY seq ASC;
