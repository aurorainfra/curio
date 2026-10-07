-- Aurora: re-apply 20260211-fix-raw-size-table.sql.
-- The remote-seal branch (20260211-remoteseal-delegated.sql) claimed 20260211
-- on some databases, leaving market_fix_raw_size with the old SERIAL id from
-- 20251231-fix-raw-size.sql. The table only holds in-flight fix task
-- scheduling, so recreate it only when the id column is not TEXT.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_name = 'market_fix_raw_size'
          AND table_schema = current_schema()
          AND column_name = 'id'
          AND data_type <> 'text'
    ) THEN
        DROP TABLE market_fix_raw_size;
    END IF;
END
$$;

CREATE TABLE IF NOT EXISTS market_fix_raw_size (
    id TEXT PRIMARY KEY,
    task_id BIGINT NOT NULL
);
