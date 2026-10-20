-- name: AcquireSharedServiceLease :one
SELECT pg_try_advisory_lock_shared(hashtextextended($1::text,0));

-- name: BindStorage :one
INSERT INTO knotra_settings(key,value) VALUES ('storage',sqlc.arg(identity)::text)
ON CONFLICT (key) DO UPDATE SET value=knotra_settings.value
WHERE knotra_settings.value=EXCLUDED.value
RETURNING value;

-- name: ReadStorageBinding :one
SELECT value FROM knotra_settings WHERE key='storage';

-- name: HasDurableData :one
SELECT (EXISTS(SELECT 1 FROM knotra_runs) OR EXISTS(SELECT 1 FROM knotra_artifacts))::boolean AS has_data;
