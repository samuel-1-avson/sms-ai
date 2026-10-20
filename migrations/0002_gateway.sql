-- Android phone gateway (TypeScript rewrite). The phone's SIM receives and
-- sends every SMS; USSD is no longer served.

DROP TABLE IF EXISTS ussd_sessions;

-- ---------------------------------------------------------------------------
-- Outbox the phone long-polls. A row is written when the dispatcher hands a
-- message to the gateway; the phone acknowledges it once saved locally and
-- reports sent/delivered/failed afterwards. The phone's own store, keyed by
-- message_id, is what prevents a double send: an unacknowledged row may be
-- offered again safely.
-- ---------------------------------------------------------------------------
CREATE TABLE gateway_outbox (
    message_id      uuid PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
    to_ciphertext   bytea       NOT NULL,
    body_ciphertext bytea       NOT NULL,
    key_version     smallint    NOT NULL,
    segments        integer     NOT NULL DEFAULT 1,
    state           text        NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'acked', 'sent', 'delivered', 'failed', 'expired')),
    offered_count   integer     NOT NULL DEFAULT 0,
    offered_at      timestamptz,
    acked_at        timestamptz,
    sent_at         timestamptz,
    finished_at     timestamptz,
    stale_flagged   boolean     NOT NULL DEFAULT false,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_gateway_outbox_queued ON gateway_outbox (created_at) WHERE state = 'queued';
CREATE INDEX ix_gateway_outbox_acked ON gateway_outbox (acked_at) WHERE state = 'acked';

-- ---------------------------------------------------------------------------
-- Gateway phone health, from its heartbeats (no message content).
-- ---------------------------------------------------------------------------
CREATE TABLE gateway_devices (
    device_id      text PRIMARY KEY,
    app_version    text        NOT NULL DEFAULT '',
    battery        real,
    charging       boolean,
    signal         smallint,
    sim_label      text,
    inbox_pending  integer     NOT NULL DEFAULT 0,
    outbox_pending integer     NOT NULL DEFAULT 0,
    status_pending integer     NOT NULL DEFAULT 0,
    paused         boolean     NOT NULL DEFAULT false,
    last_seen_at   timestamptz NOT NULL DEFAULT now()
);
