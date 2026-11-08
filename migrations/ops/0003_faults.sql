-- Fault injection settings (D13). One row per named injection point; every API process polls and caches
-- the table. `remaining` limits how many times a fault fires (NULL = unlimited); it is consumed with an
-- atomic UPDATE, so "crash once" fires in exactly one process and stays consumed after a restart.
CREATE TABLE fault_config (
    point       text             PRIMARY KEY,           -- e.g. confirm.after_payment
    action      text             NOT NULL,              -- crash | error | delay | disable
    params      jsonb            NOT NULL DEFAULT '{}', -- e.g. {"delay_ms": 500}
    remaining   int              CHECK (remaining >= 0),
    probability double precision NOT NULL DEFAULT 1 CHECK (probability > 0 AND probability <= 1),
    updated_at  timestamptz      NOT NULL DEFAULT clock_timestamp()
);
