-- Events and their items: one row per sellable unit (D2).
CREATE TABLE events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    capacity   int         NOT NULL CHECK (capacity > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE items (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id           bigint NOT NULL REFERENCES events (id),
    seq                int    NOT NULL,
    state              text   NOT NULL DEFAULT 'free' CHECK (state IN ('free', 'held', 'confirmed')),
    -- Fencing counter (D3, D4): bumped on every hold, so a stale holder can be recognised.
    hold_version       bigint NOT NULL DEFAULT 0,
    holder             text,
    hold_request_id    text,             -- client request id, makes hold retries idempotent (D5)
    hold_expires_at    timestamptz,
    payment_started_at timestamptz,      -- in-progress marker for the payment step (D5)
    UNIQUE (event_id, seq),
    CHECK ((state = 'free') = (holder IS NULL)),
    CHECK (state <> 'held' OR hold_expires_at IS NOT NULL)
);

-- Supports the shared availability predicate (D3): free, or held and expired.
CREATE INDEX items_availability ON items (event_id, state, hold_expires_at);

-- Append-only ledger: one row per confirmation. The strategies write here when they confirm;
-- the independent checker reads it.
CREATE TABLE bookings (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    item_id      bigint      NOT NULL REFERENCES items (id),
    event_id     bigint      NOT NULL REFERENCES events (id),
    buyer_id     text        NOT NULL,
    hold_version bigint      NOT NULL,   -- lets a retried confirm be recognised (D5)
    status       text        NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'cancelled')),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX bookings_item ON bookings (item_id);
