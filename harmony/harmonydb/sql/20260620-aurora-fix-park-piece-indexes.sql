-- Aurora: re-apply 20251014-park-piece-optimisation.sql.
-- Migrations are recorded by date prefix only; an older pdp branch migration
-- (20251014-pdp-piece-adds-datasetid-nullable.sql) claimed 20251014 on some
-- databases, so the upstream file never ran there. Idempotent elsewhere.
CREATE INDEX IF NOT EXISTS idx_parked_piece_refs_piece_id
    ON parked_piece_refs (piece_id);

CREATE INDEX IF NOT EXISTS idx_parked_pieces_cleanup_null
    ON parked_pieces (id) WHERE cleanup_task_id IS NULL;
