-- Bootstrap database + role for Stalwart v0.16.0.
--
-- Stalwart v0.16.0 stores *all* runtime settings (listeners, blob
-- store, directories, DKIM keys, domains, users, etc.) in a data
-- store — we point it at PostgreSQL, and this script prepares the
-- dedicated database and login role so the mail server doesn't share
-- a schema with KMail's control-plane tables.
--
-- This file is mounted read-only into the official `postgres:16`
-- image at `/docker-entrypoint-initdb.d/` by docker-compose, which
-- runs each `.sql` / `.sh` entry once on a fresh data volume (per
-- https://github.com/docker-library/docs/blob/master/postgres/README.md).
-- On re-creates with an existing `postgres_data` volume Postgres
-- skips init scripts, so these statements only run the first time.

-- Role + database for the Stalwart mail server.
CREATE ROLE stalwart WITH LOGIN PASSWORD 'stalwart-dev';
CREATE DATABASE stalwart OWNER stalwart;
GRANT ALL PRIVILEGES ON DATABASE stalwart TO stalwart;
