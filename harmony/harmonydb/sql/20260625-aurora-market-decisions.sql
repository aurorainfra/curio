-- Aurora: storage deal acceptance decisions (aurora/marketlog), kept 7 days.
-- Dated into the Aurora re-apply range so no upstream migration date can
-- collide with it (migrations are tracked by date prefix only).
CREATE TABLE IF NOT EXISTS aurora_market_decisions (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL DEFAULT now(),
    machine TEXT NOT NULL,
    market TEXT NOT NULL,
    deal_uuid TEXT NOT NULL,
    sp_id BIGINT,
    client TEXT,
    piece_cid TEXT,
    piece_size BIGINT,
    verified BOOLEAN,
    offline BOOLEAN,
    accepted BOOLEAN NOT NULL,
    reason TEXT,
    error TEXT,
    bp_mk12 BOOLEAN,
    bp_sector BOOLEAN,
    cidgravity JSONB,
    duration_ms BIGINT
);

CREATE INDEX IF NOT EXISTS aurora_market_decisions_ts ON aurora_market_decisions (ts DESC);
CREATE INDEX IF NOT EXISTS aurora_market_decisions_sp_ts ON aurora_market_decisions (sp_id, ts DESC);
