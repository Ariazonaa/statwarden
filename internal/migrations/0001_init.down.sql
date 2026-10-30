-- 0001_init.down.sql — drop the Statwarden core schema (reverse dependency order).

BEGIN;

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS status_page_subscribers;
DROP TABLE IF EXISTS incident_updates;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS status_page_monitors;
DROP TABLE IF EXISTS status_page_groups;
DROP TABLE IF EXISTS status_pages;
DROP TABLE IF EXISTS maintenance_monitors;
DROP TABLE IF EXISTS maintenance_windows;
DROP TABLE IF EXISTS monitor_notifications;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS cert_info;
DROP TABLE IF EXISTS heartbeats;
DROP TABLE IF EXISTS monitor_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS monitors;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

COMMIT;
