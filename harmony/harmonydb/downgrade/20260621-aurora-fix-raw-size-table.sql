-- No-op: this migration only re-applies an upstream migration that was
-- shadowed by a date-prefix collision; the objects belong to (and are
-- downgraded with) that upstream migration.
SELECT 1;
