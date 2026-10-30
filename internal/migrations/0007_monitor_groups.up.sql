-- 0007_monitor_groups.up.sql — organisational folders for monitors.
-- A group is presentation only: it files a monitor under a heading in the
-- monitor list and carries no runtime meaning. It is deliberately NOT the
-- monitors.parent_id relation from 0001, which the engine reads to suppress
-- alerts while a parent is down, and it is not tags either: tags are
-- many-to-many labels, a folder is a single assignment.
-- Deleting a folder must never delete the monitors filed in it, hence
-- SET NULL rather than CASCADE.

BEGIN;

CREATE TABLE monitor_groups (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL UNIQUE,
    sort_order integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE monitors
    ADD COLUMN group_id bigint REFERENCES monitor_groups (id) ON DELETE SET NULL;

CREATE INDEX monitors_group_id_idx ON monitors (group_id);

COMMIT;
