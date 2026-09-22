CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_supplement_task_ordinal_uidx
    ON task_supplement (task_id, ordinal);
