-- name: LockStoreSchema :execresult
SELECT pg_advisory_xact_lock(712865723);

-- name: SchemaInitialized :one
SELECT (to_regclass('knotra_settings') IS NOT NULL)::boolean AS initialized;

-- name: InsertEngineID :execresult
INSERT INTO knotra_settings(key,value) VALUES ('engine_id',$1) ON CONFLICT DO NOTHING;

-- name: ReadEngineID :one
SELECT value FROM knotra_settings WHERE key='engine_id';

-- name: ReadCommandReceipt :one
SELECT route,digest,status,response FROM knotra_commands WHERE principal=$1 AND id=$2;

-- name: InsertCommandReceipt :execresult
INSERT INTO knotra_commands(principal,id,route,digest,status,response) VALUES($1,$2,$3,$4,$5,$6);

-- name: PutDefinition :one
INSERT INTO knotra_definitions(id,digest,document)
VALUES($1,$2,$3)
ON CONFLICT(digest) DO UPDATE
SET digest=EXCLUDED.digest
RETURNING document;

-- name: ReadDefinition :one
SELECT document FROM knotra_definitions WHERE id=$1;

-- name: ReadRunProjection :one
SELECT document FROM knotra_runs WHERE id=$1;

-- name: ReadPlan :one
SELECT plan FROM knotra_runs WHERE id=$1;

-- name: InsertRun :execresult
INSERT INTO knotra_runs(id,definition_id,document,plan,inputs,scheduler_version,state_format_version,admitted_at,execution_deadline,wake_generation,dirty_since,session_affinity)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$8,$10);

-- name: EventIDExists :one
SELECT EXISTS(SELECT 1 FROM knotra_events WHERE run_id=$1 AND id=$2);

-- name: ListEvents :many
SELECT id,document
FROM knotra_events
WHERE run_id=sqlc.arg(run_id)
    AND id>sqlc.arg(id)
    AND (sqlc.arg(instance_id)::text='' OR document->>'instanceId'=sqlc.arg(instance_id))
ORDER BY id
LIMIT 100;

-- name: ReadRunInputs :one
SELECT inputs FROM knotra_runs WHERE id=$1;

-- name: ReadRunStatus :one
SELECT (document->>'status')::text FROM knotra_runs WHERE id=$1;

-- name: ListDefinitions :many
-- Bound raw documents before sqlc materializes them; keep one cursor marker.
WITH candidates AS (
    SELECT id,octet_length(document::text) AS bytes
    FROM knotra_definitions
    WHERE (sqlc.arg(cursor)::text='' OR id<sqlc.arg(cursor))
    ORDER BY id DESC
    LIMIT 101
), page AS (
    SELECT id,row_number() OVER (ORDER BY id DESC) AS position,
        COALESCE(sum(bytes) OVER (ORDER BY id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS preceding_bytes
    FROM candidates
)
SELECT d.document
FROM knotra_definitions d JOIN page p ON p.id=d.id
WHERE p.position<=2 OR p.preceding_bytes<=sqlc.arg(max_bytes)::bigint
ORDER BY d.id DESC;

-- name: ListRuns :many
-- Bound complete root outputs before sqlc materializes them; keep one cursor marker.
WITH candidates AS (
    SELECT id,octet_length(document::text) AS bytes
    FROM knotra_runs
    WHERE (sqlc.arg(cursor)::text='' OR id<sqlc.arg(cursor))
    ORDER BY id DESC
    LIMIT 101
), page AS (
    SELECT id,row_number() OVER (ORDER BY id DESC) AS position,
        COALESCE(sum(bytes) OVER (ORDER BY id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS preceding_bytes
    FROM candidates
)
SELECT d.document
FROM knotra_runs d JOIN page p ON p.id=d.id
WHERE p.position<=2 OR p.preceding_bytes<=sqlc.arg(max_bytes)::bigint
ORDER BY d.id DESC;

-- name: ListInstances :many
WITH candidates AS (
    SELECT id,octet_length(document::text) AS bytes
    FROM knotra_instances
    WHERE run_id=sqlc.arg(run_id) AND id>sqlc.arg(cursor)::text
    ORDER BY id
    LIMIT 101
), page AS (
    SELECT id,row_number() OVER (ORDER BY id) AS position,
        COALESCE(sum(bytes) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS preceding_bytes
    FROM candidates
)
SELECT d.document
FROM knotra_instances d JOIN page p ON p.id=d.id
WHERE d.run_id=sqlc.arg(run_id) AND (p.position<=2 OR p.preceding_bytes<=sqlc.arg(max_bytes)::bigint)
ORDER BY d.id;
