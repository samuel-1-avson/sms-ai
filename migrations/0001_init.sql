-- SMS AI Ghana — initial schema (plan v2.0 §8).
-- All user content (phone numbers, message bodies, overflow buffers, USSD
-- answers) is stored as AES-256-GCM ciphertext tagged with key_version so
-- keys can be rotated online.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------
-- Users: pseudonymous identity keyed by a versioned HMAC of the E.164 number.
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_ciphertext  bytea       NOT NULL,
    phone_lookup_hash text        NOT NULL,
    key_version       smallint    NOT NULL,
    carrier           text        NOT NULL DEFAULT 'unknown',
    opted_out         boolean     NOT NULL DEFAULT false,
    opted_out_at      timestamptz,
    bonus_sms_quota   integer     NOT NULL DEFAULT 0 CHECK (bonus_sms_quota >= 0),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Supports zero-downtime HMAC key rotation lookups (plan §8, index 2).
CREATE UNIQUE INDEX ix_users_phone_lookup ON users (phone_lookup_hash, key_version);

-- ---------------------------------------------------------------------------
-- Conversations: one per (user, service, channel).
-- ---------------------------------------------------------------------------
CREATE TABLE conversations (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    service_id           text        NOT NULL,
    channel_type         text        NOT NULL CHECK (channel_type IN ('sms', 'ussd')),
    provider_name        text        NOT NULL,
    status               text        NOT NULL DEFAULT 'active',
    -- Encrypted JSON {"rest": "...", "pages_left": n} for the MORE command.
    overflow_buffer      bytea,
    overflow_key_version smallint,
    overflow_expires_at  timestamptz,
    next_sequence        bigint      NOT NULL DEFAULT 1,
    context_started_at   timestamptz NOT NULL DEFAULT now(),
    context_expires_at   timestamptz NOT NULL DEFAULT now(),
    last_activity_at     timestamptz NOT NULL DEFAULT now(),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX ux_conversations_user_service ON conversations (user_id, service_id, channel_type);

-- ---------------------------------------------------------------------------
-- Jobs: DB-backed queue with inbound debounce/coalescing (plan §8).
-- ---------------------------------------------------------------------------
CREATE TABLE jobs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    kind              text        NOT NULL CHECK (kind IN ('answer', 'send')),
    status            text        NOT NULL CHECK (status IN ('pending', 'running', 'done', 'failed', 'cancelled')),
    latest_message_id uuid,
    reply_message_id  uuid,
    coalesce_count    integer     NOT NULL DEFAULT 0,
    allow_ucs2        boolean     NOT NULL DEFAULT false,
    credit_source     text CHECK (credit_source IN ('free', 'bonus')),
    credit_day        date,
    result            text,
    attempts          integer     NOT NULL DEFAULT 0,
    max_attempts      integer     NOT NULL DEFAULT 3,
    available_at      timestamptz NOT NULL DEFAULT now(),
    lease_owner       text,
    lease_expires_at  timestamptz,
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz
);

-- Claimable jobs index (plan §8, index 4).
CREATE INDEX ix_jobs_claimable ON jobs (status, available_at) WHERE status = 'pending';
CREATE INDEX ix_jobs_running ON jobs (conversation_id, lease_expires_at) WHERE status = 'running';
-- At most one pending answer job per conversation: new inbound texts are
-- coalesced into it instead of creating a second LLM call.
CREATE UNIQUE INDEX ux_jobs_one_pending_answer ON jobs (conversation_id) WHERE status = 'pending' AND kind = 'answer';

-- ---------------------------------------------------------------------------
-- Messages: inbound and outbound, bodies encrypted.
-- ---------------------------------------------------------------------------
CREATE TABLE messages (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id      uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    sequence             bigint      NOT NULL,
    job_id               uuid REFERENCES jobs (id) ON DELETE SET NULL,
    provider_name        text        NOT NULL,
    provider_message_id  text,
    direction            text        NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    -- question | command | answer | more | command_reply | notice
    kind                 text        NOT NULL,
    -- channel the message travelled on: an answer to a USSD question can be
    -- delivered inline ('ussd') or by MT SMS ('sms').
    delivery_channel     text        NOT NULL CHECK (delivery_channel IN ('sms', 'ussd')),
    body_ciphertext      bytea       NOT NULL,
    key_version          smallint    NOT NULL,
    encoding_type        text CHECK (encoding_type IN ('gsm7', 'ucs2')),
    septet_or_char_count integer,
    segment_count        integer     NOT NULL DEFAULT 0,
    answer_source        text,
    status               text        NOT NULL,
    error_code           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- 1. Prevent duplicate inbound webhook processing (plan §8, index 1).
CREATE UNIQUE INDEX ux_inbound_provider_message
    ON messages (provider_name, provider_message_id)
    WHERE direction = 'inbound' AND provider_message_id IS NOT NULL;

-- 3. Fast recent context retrieval (plan §8, index 3).
CREATE INDEX ix_conversation_recent_messages ON messages (conversation_id, created_at DESC);

-- Delivery receipts can arrive late, repeated or out of order: a status may
-- only move to a higher rank (never delivered -> submitted).
CREATE FUNCTION message_status_rank(s text) RETURNS integer
    LANGUAGE sql IMMUTABLE AS
$$
SELECT CASE s
           WHEN 'queued' THEN 0
           WHEN 'sending' THEN 1
           WHEN 'unknown_timeout' THEN 1
           WHEN 'submitted' THEN 2
           WHEN 'delivered' THEN 3
           WHEN 'failed' THEN 3
           WHEN 'expired' THEN 3
           WHEN 'rejected' THEN 3
           ELSE 0
       END
$$;

CREATE INDEX ix_messages_outbound_provider_id
    ON messages (provider_name, provider_message_id)
    WHERE direction = 'outbound' AND provider_message_id IS NOT NULL;
CREATE INDEX ix_messages_job ON messages (job_id) WHERE job_id IS NOT NULL;
CREATE INDEX ix_messages_created_at ON messages (created_at);

ALTER TABLE jobs
    ADD CONSTRAINT fk_jobs_latest_message FOREIGN KEY (latest_message_id) REFERENCES messages (id) ON DELETE SET NULL,
    ADD CONSTRAINT fk_jobs_reply_message FOREIGN KEY (reply_message_id) REFERENCES messages (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- Outbound send attempts (transactional outbox; never resend blindly).
-- ---------------------------------------------------------------------------
CREATE TABLE send_attempts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id          uuid        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    provider_name       text        NOT NULL,
    provider_request_id text        NOT NULL,
    provider_message_id text,
    status              text        NOT NULL CHECK (status IN ('pending', 'submitted', 'failed', 'unknown_timeout', 'reconciled_submitted', 'reconciled_not_found')),
    error               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_send_attempts_message ON send_attempts (message_id);
CREATE INDEX ix_send_attempts_unknown ON send_attempts (status) WHERE status IN ('pending', 'unknown_timeout');

-- ---------------------------------------------------------------------------
-- Model calls: token usage and cost ledger (no content).
-- ---------------------------------------------------------------------------
CREATE TABLE model_calls (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id             uuid REFERENCES jobs (id) ON DELETE SET NULL,
    message_id         uuid REFERENCES messages (id) ON DELETE SET NULL,
    channel            text        NOT NULL,
    model_name         text        NOT NULL,
    status             text        NOT NULL,
    input_tokens       integer     NOT NULL DEFAULT 0,
    output_tokens      integer     NOT NULL DEFAULT 0,
    reasoning_tokens   integer     NOT NULL DEFAULT 0,
    estimated_cost_usd numeric(14, 8) NOT NULL DEFAULT 0,
    latency_ms         integer     NOT NULL DEFAULT 0,
    error              text,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_model_calls_created_at ON model_calls (created_at);

-- ---------------------------------------------------------------------------
-- Local Ghana Grounding Store (plan §7.5).
-- ---------------------------------------------------------------------------
CREATE TABLE knowledge_snippets (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                 text        NOT NULL UNIQUE,
    category             text        NOT NULL,
    -- Trigger phrases, one per line.
    trigger_keywords     text        NOT NULL,
    verified_answer_gsm7 text        NOT NULL CHECK (char_length(verified_answer_gsm7) <= 160),
    context_note         text,
    direct_reply_enabled boolean     NOT NULL DEFAULT true,
    reviewed             boolean     NOT NULL DEFAULT false,
    source               text,
    enabled              boolean     NOT NULL DEFAULT true,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- 5. Fast trigram search on the grounding store (plan §8, index 5).
CREATE INDEX ix_knowledge_snippets_trgm ON knowledge_snippets USING gin (trigger_keywords gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- Sponsored quota and vouchers (plan §11.3).
-- ---------------------------------------------------------------------------
CREATE TABLE sponsored_pools (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    voucher_code   text        NOT NULL UNIQUE CHECK (voucher_code = upper(voucher_code)),
    sponsor_name   text        NOT NULL,
    max_segments   integer     NOT NULL CHECK (max_segments > 0),
    used_segments  integer     NOT NULL DEFAULT 0 CHECK (used_segments >= 0),
    per_user_grant integer     NOT NULL CHECK (per_user_grant > 0),
    active         boolean     NOT NULL DEFAULT true,
    expires_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (used_segments <= max_segments)
);

CREATE TABLE voucher_redemptions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id          uuid        NOT NULL REFERENCES sponsored_pools (id) ON DELETE CASCADE,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    granted_segments integer     NOT NULL,
    redeemed_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (pool_id, user_id)
);

-- ---------------------------------------------------------------------------
-- Quota and budget ledgers.
-- ---------------------------------------------------------------------------
CREATE TABLE usage_daily (
    user_id              uuid    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    day                  date    NOT NULL,
    questions_used       integer NOT NULL DEFAULT 0,
    bonus_used           integer NOT NULL DEFAULT 0,
    limit_notice_sent    boolean NOT NULL DEFAULT false,
    outage_notice_sent   boolean NOT NULL DEFAULT false,
    failed_code_attempts integer NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

CREATE TABLE global_usage_daily (
    day          date PRIMARY KEY,
    sms_segments integer        NOT NULL DEFAULT 0,
    ai_cost_usd  numeric(14, 8) NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- USSD session state (Redis later, plan §12.2 P2).
-- ---------------------------------------------------------------------------
CREATE TABLE ussd_sessions (
    provider_name           text        NOT NULL,
    session_id              text        NOT NULL,
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    conversation_id         uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    state                   text        NOT NULL,
    last_answer_ciphertext  bytea,
    last_answer_key_version smallint,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    expires_at              timestamptz NOT NULL,
    PRIMARY KEY (provider_name, session_id)
);

CREATE INDEX ix_ussd_sessions_expires ON ussd_sessions (expires_at);

-- ---------------------------------------------------------------------------
-- Operator kill switches.
-- ---------------------------------------------------------------------------
CREATE TABLE system_flags (
    key        text PRIMARY KEY,
    value      boolean     NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO system_flags (key, value) VALUES
    ('ai_enabled', true),
    ('sms_outbound_enabled', true);
