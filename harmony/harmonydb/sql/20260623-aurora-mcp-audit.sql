-- Aurora: tool-call log of the read-only diagnostics MCP (aurora/mcp).
-- Dated into the Aurora re-apply range so no upstream migration date can
-- collide with it (migrations are tracked by date prefix only).
CREATE TABLE IF NOT EXISTS aurora_mcp_calls (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL DEFAULT now(),
    agent TEXT NOT NULL,
    machine_id INT,
    tool TEXT NOT NULL,
    args JSONB NOT NULL,
    duration_ms BIGINT NOT NULL,
    result_bytes BIGINT NOT NULL,
    truncated BOOLEAN NOT NULL,
    error TEXT
);

CREATE INDEX IF NOT EXISTS aurora_mcp_calls_ts ON aurora_mcp_calls (ts DESC);
