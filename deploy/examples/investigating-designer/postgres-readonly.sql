-- Read-only PostgreSQL identity for the investigating designer
-- (docs/INVESTIGATING_DESIGNER.md §3.3, layer 1).
--
-- The body of the guard is what this role is GRANTed: SELECT on
-- content-free views, and nothing else. Session settings such as
-- transaction_read_only are auxiliary (any session can SET them off)
-- and are not counted as protection.
--
-- Placeholders: <db> <app_schema> <owner_role> <migration_role> <app_role>
-- <other_db>.
-- Run as a superuser or the database owner. On managed PostgreSQL, where
-- no superuser is available, the executing role must also be a member of
-- <owner_role> and <migration_role> (or be that role): otherwise the
-- ALTER ... OWNER TO and ALTER DEFAULT PRIVILEGES FOR ROLE statements
-- below are refused. GRANT <owner_role>, <migration_role> TO the
-- executing role first.
-- Review every REVOKE ... FROM PUBLIC: it removes the default grant for
-- every role. Section 4 gives EXECUTE back to <app_role>, now and for
-- future functions; check those lines against the roles the application
-- really connects, migrates and reports with, and repeat them per role.

-- 1. The role: login only. No ownership, no CREATE, no memberships.
--    Never grant it a predefined role: pg_read_all_data (reads past the
--    views), pg_read_all_stats and pg_monitor (query text with literal
--    values, in pg_stat_activity and pg_stat_statements),
--    pg_read_server_files, pg_write_server_files and
--    pg_execute_server_program (the server's file system and shell),
--    pg_signal_backend (cancels other sessions).
CREATE ROLE lassdas_investigator LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  CONNECTION LIMIT 2
  PASSWORD '<set-by-the-operator>';
ALTER ROLE lassdas_investigator SET default_transaction_read_only = on;   -- auxiliary
ALTER ROLE lassdas_investigator SET statement_timeout = '10s';            -- auxiliary
ALTER ROLE lassdas_investigator SET lock_timeout = '2s';                  -- auxiliary

REVOKE ALL ON DATABASE <db> FROM lassdas_investigator;
GRANT CONNECT ON DATABASE <db> TO lassdas_investigator;
-- PUBLIC holds CONNECT (and TEMP) on every database by default, and the
-- REVOKE above touches <db> only: the role can still log in to the other
-- databases of this cluster, where nothing below has run. Either run this
-- script in each of them, or take PUBLIC's CONNECT away per other database
-- (then GRANT CONNECT back to the roles that use it), or record that the
-- cluster's policy already does so. One line per other database:
-- REVOKE CONNECT ON DATABASE <other_db> FROM PUBLIC;

-- 2. A schema for the views. The investigator gets USAGE here; `public`
--    keeps its default USAGE for every role (section 4 covers it), and
--    <app_schema> is withheld.
CREATE SCHEMA IF NOT EXISTS lassdas_ro AUTHORIZATION <owner_role>;
GRANT USAGE ON SCHEMA lassdas_ro TO lassdas_investigator;
REVOKE ALL ON SCHEMA <app_schema> FROM lassdas_investigator;

-- 2b. CREATE on public. Through PostgreSQL 14, and in clusters upgraded
--    or restored from those versions, PUBLIC holds CREATE on the public
--    schema, so the role could CREATE TABLE public.x (only the auxiliary
--    read-only default would stop it). Take the default away and give
--    CREATE back to the roles that create objects in public. If the
--    server answers that no privileges could be revoked, the schema
--    belongs to a role you are not a member of: run the line as that
--    role. Check as the investigator: BEGIN; SET TRANSACTION READ WRITE;
--    CREATE TABLE public.lassdas_probe(); must end in "permission denied
--    for schema public" (then ROLLBACK).
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT  CREATE ON SCHEMA public TO <owner_role>, <migration_role>;   -- drop when neither creates in public

-- 3. Content-free views: counts, timestamps, states, ids. No column that
--    carries customer content. Views run with their owner's privileges, so
--    the investigator never needs SELECT on the tables themselves, and so
--    the owner must be <owner_role>, not the operator running this script
--    (a superuser owner would read past row security on the tables, and
--    the default privileges below apply only to views <owner_role>
--    creates). One example; add one view per table the role may look at,
--    each followed by the same ALTER VIEW ... OWNER TO, or create them as
--    <owner_role> (SET ROLE <owner_role>) from the start.
CREATE OR REPLACE VIEW lassdas_ro.<table>_shape AS
  SELECT id, created_at, updated_at, state
  FROM <app_schema>.<table>;
ALTER VIEW lassdas_ro.<table>_shape OWNER TO <owner_role>;
GRANT SELECT ON ALL TABLES IN SCHEMA lassdas_ro TO lassdas_investigator;
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role> IN SCHEMA lassdas_ro
  GRANT SELECT ON TABLES TO lassdas_investigator;

-- 4. Functions. PostgreSQL grants EXECUTE on functions to PUBLIC by
--    default, so a SELECT-only role can still call `SELECT writer_fn()`
--    and a SECURITY DEFINER function writes with its owner's rights.
--    Revoke the public grant in every schema the role has USAGE on
--    (`public` has USAGE for everyone by default) and keep it revoked for
--    functions created later, per creating role and schema. The revocation
--    hits every role, so the application role gets EXECUTE back
--    explicitly: for the functions and procedures that exist, and as a
--    default for the ones its migrations add later. Extension functions
--    (uuid-ossp, pgcrypto, pg_trgm, ...) usually live in public, which is
--    why public is in the re-grant. Review the <app_role> lines against
--    the roles the application actually uses and repeat them per role.
REVOKE EXECUTE ON ALL FUNCTIONS  IN SCHEMA public, lassdas_ro, <app_schema> FROM PUBLIC;
REVOKE EXECUTE ON ALL PROCEDURES IN SCHEMA public, lassdas_ro, <app_schema> FROM PUBLIC;
GRANT  EXECUTE ON ALL FUNCTIONS  IN SCHEMA public, <app_schema> TO <app_role>;   -- the application keeps working
GRANT  EXECUTE ON ALL PROCEDURES IN SCHEMA public, <app_schema> TO <app_role>;
-- Defaults for future functions. These have to be the creating role's
-- GLOBAL defaults (no IN SCHEMA): PostgreSQL adds per-schema default
-- privileges to the global ones and never subtracts, so a REVOKE ... FROM
-- PUBLIC written IN SCHEMA changes nothing and the built-in PUBLIC EXECUTE
-- keeps applying to every function the role creates afterwards (stage-0
-- row 10 in README.md is what catches this: a SECURITY DEFINER writer
-- created after a per-schema version was still callable by the
-- investigator on a staging rehearsal, 2026-09-05). Each REVOKE is paired
-- with the GRANT that keeps the application able to call what its
-- migrations create. FUNCTIONS here covers procedures too (default
-- privileges cannot tell them apart). The global default reaches every
-- schema the role creates functions in, lassdas_ro included, which is why
-- there is no separate lassdas_ro line.
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role>     REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE <owner_role>     GRANT  EXECUTE ON FUNCTIONS TO <app_role>;
ALTER DEFAULT PRIVILEGES FOR ROLE <migration_role> REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE <migration_role> GRANT  EXECUTE ON FUNCTIONS TO <app_role>;
-- Repeat the paired lines for every other role that creates functions.

-- 5. Extensions that reach outside the database. If installed, their
--    functions live in a schema covered above; make sure that schema is
--    in the REVOKE list, and never grant the investigator USAGE on it.
--    dblink, postgres_fdw, file_fdw, adminpack are the usual ones.
--    pg_read_file & co. require pg_read_server_files, one of the
--    predefined roles section 1 never grants.

-- 6. Staging only. When the consumer declares
--    design.staging_has_no_customer_content = true, content columns may
--    be exposed through additional views in the same schema, owned by
--    <owner_role> as in section 3:
-- CREATE OR REPLACE VIEW lassdas_ro.<table>_content AS SELECT * FROM <app_schema>.<table>;
-- ALTER VIEW lassdas_ro.<table>_content OWNER TO <owner_role>;
