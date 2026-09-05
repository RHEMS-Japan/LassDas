-- Read-only PostgreSQL identity for the investigating designer
-- (docs/INVESTIGATING_DESIGNER.md §3.3, layer 1).
--
-- The body of the guard is what this role is GRANTed: SELECT on
-- content-free views, and nothing else. Session settings such as
-- transaction_read_only are auxiliary (any session can SET them off)
-- and are not counted as protection.
--
-- Placeholders: <db> <app_schema> <owner_role> <migration_role> <app_role>.
-- Run as a superuser or the database owner. Review every REVOKE ... FROM
-- PUBLIC: it removes the default grant for every role, so re-grant EXECUTE
-- to the application role where it needs it.

-- 1. The role: login only. No ownership, no CREATE, no default roles.
CREATE ROLE lassdas_investigator LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  CONNECTION LIMIT 2
  PASSWORD '<set-by-the-operator>';
ALTER ROLE lassdas_investigator SET default_transaction_read_only = on;   -- auxiliary
ALTER ROLE lassdas_investigator SET statement_timeout = '10s';            -- auxiliary
ALTER ROLE lassdas_investigator SET lock_timeout = '2s';                  -- auxiliary

REVOKE ALL ON DATABASE <db> FROM lassdas_investigator;
GRANT CONNECT ON DATABASE <db> TO lassdas_investigator;

-- 2. A schema for the views. The investigator gets USAGE here and nowhere else.
CREATE SCHEMA IF NOT EXISTS lassdas_ro AUTHORIZATION <owner_role>;
GRANT USAGE ON SCHEMA lassdas_ro TO lassdas_investigator;
REVOKE ALL ON SCHEMA <app_schema> FROM lassdas_investigator;

-- 3. Content-free views: counts, timestamps, states, ids. No column that
--    carries customer content. Views run with the owner's privileges, so
--    the investigator never needs SELECT on the tables themselves.
--    One example; add one view per table the role may look at.
CREATE OR REPLACE VIEW lassdas_ro.<table>_shape AS
  SELECT id, created_at, updated_at, state
  FROM <app_schema>.<table>;
GRANT SELECT ON ALL TABLES IN SCHEMA lassdas_ro TO lassdas_investigator;
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role> IN SCHEMA lassdas_ro
  GRANT SELECT ON TABLES TO lassdas_investigator;

-- 4. Functions. PostgreSQL grants EXECUTE on functions to PUBLIC by
--    default, so a SELECT-only role can still call `SELECT writer_fn()`
--    and a SECURITY DEFINER function writes with its owner's rights.
--    Revoke the public grant in every schema the role has USAGE on
--    (`public` has USAGE for everyone by default) and keep it revoked for
--    functions created later, per creating role and schema.
REVOKE EXECUTE ON ALL FUNCTIONS  IN SCHEMA public, lassdas_ro, <app_schema> FROM PUBLIC;
REVOKE EXECUTE ON ALL PROCEDURES IN SCHEMA public, lassdas_ro, <app_schema> FROM PUBLIC;
GRANT  EXECUTE ON ALL FUNCTIONS  IN SCHEMA <app_schema> TO <app_role>;   -- the application keeps working
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role>     IN SCHEMA public      REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role>     IN SCHEMA <app_schema> REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE <migration_role> IN SCHEMA public      REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE <migration_role> IN SCHEMA <app_schema> REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
-- Repeat the two ALTER DEFAULT PRIVILEGES lines for every other role that creates functions.

-- 5. Extensions that reach outside the database. If installed, their
--    functions live in a schema covered above; make sure that schema is
--    in the REVOKE list, and never grant the investigator USAGE on it.
--    dblink, postgres_fdw, file_fdw, adminpack are the usual ones.
--    pg_read_file & co. require pg_read_server_files, which this role is
--    never granted.

-- 6. Staging only. When the consumer declares
--    design.staging_has_no_customer_content = true, content columns may
--    be exposed through additional views in the same schema:
-- CREATE OR REPLACE VIEW lassdas_ro.<table>_content AS SELECT * FROM <app_schema>.<table>;
