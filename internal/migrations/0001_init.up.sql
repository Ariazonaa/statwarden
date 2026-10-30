-- 0001_init.up.sql — Statwarden core schema.
-- Tables per docs/architecture.md: users, sessions, monitors, tags,
-- monitor_tags, heartbeats, cert_info, notifications, monitor_notifications,
-- maintenance_windows, maintenance_monitors, status_pages, status_page_groups,
-- status_page_monitors, incidents, incident_updates, status_page_subscribers,
-- settings, audit_log.

BEGIN;

-- ---------------------------------------------------------------------------
-- Users & sessions
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    display_name  text        NOT NULL DEFAULT '',
    role          text        NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user', 'viewer')),
    totp_secret   text,
    totp_enabled  boolean     NOT NULL DEFAULT false,
    backup_codes  jsonb,
    avatar_path   text,
    locale        text,
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_lower_idx ON users (lower(email));

-- Server-side sessions; id is the (hashed) opaque cookie token.
CREATE TABLE sessions (
    id           text PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    ip           inet,
    user_agent   text
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- Monitors, tags & heartbeats
-- ---------------------------------------------------------------------------

CREATE TABLE monitors (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                    text        NOT NULL,
    type                    text        NOT NULL, -- registry type name (http, tcp, ping, dns, push, ...)
    config                  jsonb       NOT NULL DEFAULT '{}'::jsonb, -- validated by the type's Checker.Validate()
    interval_seconds        integer     NOT NULL DEFAULT 60 CHECK (interval_seconds >= 1),
    timeout_seconds         integer     CHECK (timeout_seconds IS NULL OR timeout_seconds >= 1), -- NULL = per-type default
    max_retries             integer     NOT NULL DEFAULT 0 CHECK (max_retries >= 0),
    retry_interval_seconds  integer     NOT NULL DEFAULT 60 CHECK (retry_interval_seconds >= 1),
    resend_interval_minutes integer     NOT NULL DEFAULT 0 CHECK (resend_interval_minutes >= 0), -- 0 = never resend
    upside_down             boolean     NOT NULL DEFAULT false,
    active                  boolean     NOT NULL DEFAULT true, -- false = paused
    status                  text        NOT NULL DEFAULT 'pending' CHECK (status IN ('up', 'down', 'pending', 'maintenance', 'paused')),
    parent_id               bigint      REFERENCES monitors (id) ON DELETE SET NULL,
    push_token              text,
    description             text        NOT NULL DEFAULT '',
    link_url                text        NOT NULL DEFAULT '',
    created_by              bigint      REFERENCES users (id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX monitors_push_token_idx ON monitors (push_token) WHERE push_token IS NOT NULL;
CREATE INDEX monitors_parent_id_idx ON monitors (parent_id);
CREATE INDEX monitors_type_idx ON monitors (type);

CREATE TABLE tags (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL UNIQUE,
    color      text        NOT NULL DEFAULT '#8b8fa3',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE monitor_tags (
    monitor_id bigint NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    tag_id     bigint NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, tag_id)
);

CREATE TABLE heartbeats (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    monitor_id bigint      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    checked_at timestamptz NOT NULL DEFAULT now(),
    status     text        NOT NULL CHECK (status IN ('up', 'down', 'pending', 'maintenance')),
    latency_ms integer,
    message    text        NOT NULL DEFAULT '',
    important  boolean     NOT NULL DEFAULT false, -- true on state transitions (event log)
    retries    integer     NOT NULL DEFAULT 0,
    extra      jsonb
);

CREATE INDEX heartbeats_monitor_id_checked_at_idx ON heartbeats (monitor_id, checked_at DESC);
CREATE INDEX heartbeats_important_idx ON heartbeats (monitor_id, checked_at DESC) WHERE important;

CREATE TABLE cert_info (
    monitor_id         bigint PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    subject            text,
    issuer             text,
    serial             text,
    fingerprint_sha256 text,
    valid_from         timestamptz,
    valid_to           timestamptz,
    days_remaining     integer,
    details            jsonb,
    checked_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cert_info_valid_to_idx ON cert_info (valid_to);

-- ---------------------------------------------------------------------------
-- Notifications
-- ---------------------------------------------------------------------------

CREATE TABLE notifications (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    type       text        NOT NULL, -- provider registry name (smtp, slack, webhook, ...)
    config     jsonb       NOT NULL DEFAULT '{}'::jsonb, -- secrets AES-GCM-encrypted at the app layer
    is_default boolean     NOT NULL DEFAULT false, -- auto-attach to new monitors
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE monitor_notifications (
    monitor_id      bigint NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    notification_id bigint NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, notification_id)
);

-- ---------------------------------------------------------------------------
-- Maintenance windows
-- ---------------------------------------------------------------------------

CREATE TABLE maintenance_windows (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    active      boolean     NOT NULL DEFAULT true,
    strategy    text        NOT NULL DEFAULT 'single' CHECK (strategy IN ('single', 'recurring')),
    starts_at   timestamptz,
    ends_at     timestamptz,
    recurrence  jsonb, -- weekday/cron spec for recurring windows
    timezone    text        NOT NULL DEFAULT 'UTC',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE maintenance_monitors (
    maintenance_id bigint NOT NULL REFERENCES maintenance_windows (id) ON DELETE CASCADE,
    monitor_id     bigint NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    PRIMARY KEY (maintenance_id, monitor_id)
);

-- ---------------------------------------------------------------------------
-- Status pages & incidents
-- ---------------------------------------------------------------------------

CREATE TABLE status_pages (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug           text        NOT NULL UNIQUE,
    title          text        NOT NULL,
    description    text        NOT NULL DEFAULT '',
    footer_text    text        NOT NULL DEFAULT '',
    theme          text        NOT NULL DEFAULT 'auto' CHECK (theme IN ('light', 'dark', 'auto')),
    logo_path      text,
    custom_domain  text,
    published      boolean     NOT NULL DEFAULT true,
    password_hash  text, -- NULL = public
    allow_indexing boolean     NOT NULL DEFAULT true,
    locale         text,
    view_count     bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX status_pages_custom_domain_idx ON status_pages (lower(custom_domain)) WHERE custom_domain IS NOT NULL;

CREATE TABLE status_page_groups (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status_page_id bigint  NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    name           text    NOT NULL,
    sort_order     integer NOT NULL DEFAULT 0
);

CREATE INDEX status_page_groups_page_idx ON status_page_groups (status_page_id);

CREATE TABLE status_page_monitors (
    status_page_id bigint  NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    monitor_id     bigint  NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    group_id       bigint  REFERENCES status_page_groups (id) ON DELETE SET NULL,
    sort_order     integer NOT NULL DEFAULT 0,
    show_uptime    boolean NOT NULL DEFAULT true,
    PRIMARY KEY (status_page_id, monitor_id)
);

CREATE TABLE incidents (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status_page_id bigint      NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    title          text        NOT NULL,
    body           text        NOT NULL DEFAULT '', -- markdown
    severity       text        NOT NULL DEFAULT 'minor' CHECK (severity IN ('minor', 'major', 'critical')),
    status         text        NOT NULL DEFAULT 'investigating' CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    started_at     timestamptz NOT NULL DEFAULT now(),
    resolved_at    timestamptz,
    created_by     bigint      REFERENCES users (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX incidents_page_started_idx ON incidents (status_page_id, started_at DESC);

CREATE TABLE incident_updates (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    incident_id bigint      NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    status      text        NOT NULL CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    body        text        NOT NULL, -- markdown
    created_by  bigint      REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX incident_updates_incident_idx ON incident_updates (incident_id, created_at);

CREATE TABLE status_page_subscribers (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status_page_id    bigint      NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    email             text        NOT NULL,
    verified          boolean     NOT NULL DEFAULT false,
    verify_token      text,
    unsubscribe_token text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    verified_at       timestamptz,
    UNIQUE (status_page_id, email)
);

CREATE UNIQUE INDEX status_page_subscribers_verify_token_idx ON status_page_subscribers (verify_token) WHERE verify_token IS NOT NULL;
CREATE UNIQUE INDEX status_page_subscribers_unsubscribe_token_idx ON status_page_subscribers (unsubscribe_token);

-- ---------------------------------------------------------------------------
-- Settings & audit log
-- ---------------------------------------------------------------------------

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     bigint REFERENCES users (id) ON DELETE SET NULL,
    action      text        NOT NULL, -- e.g. monitor.create, user.login
    entity_type text,
    entity_id   bigint,
    details     jsonb,
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_created_at_idx ON audit_log (created_at DESC);
CREATE INDEX audit_log_user_id_idx ON audit_log (user_id);

COMMIT;
