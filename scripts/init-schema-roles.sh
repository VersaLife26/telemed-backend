#!/usr/bin/env bash
#
# Per-schema roles for the consolidated database.
#
# WHAT THIS REPLACES
# Before consolidation each domain owned a whole database and had two roles:
# telemed_<domain>_mig (owns the objects, runs migrations) and
# telemed_<domain>_app (DML only, owns nothing). Isolation was enforced by
# CONNECT privilege -- telemed_payment_app simply could not open
# telemed_user, so a compromised payment path could not read the user
# directory. That is security review F14.
#
# One database cannot enforce it that way. This script rebuilds the same
# boundary one level down, on schemas:
#
#   * every domain keeps BOTH roles, exactly as before;
#   * the app role gets USAGE on its own schema and nothing on any other;
#   * REVOKE ALL ON SCHEMA ... FROM PUBLIC is what makes that hold, because
#     PUBLIC has USAGE on a new schema by default and a default grant is not
#     visible in a review of the explicit ones;
#   * the mig role owns everything, so the app role can never DROP, ALTER or
#     re-GRANT what a migration revoked.
#
# WHILE THERE ARE STILL NINE BINARIES (phase 2) THIS IS EQUIVALENT.
# Each process still connects as its own role, so the boundary is as strong as
# it was -- it is enforced by the server, not by application code.
#
# THAT CHANGES IN PHASE 3. One binary has one connection identity. Either it
# connects as a single role holding USAGE on all eight schemas -- which is the
# loss recorded as L1 in the consolidation plan -- or it holds a connection
# pool per domain, each as that domain's own role, and keeps this boundary
# intact. The second is strictly better and is what these roles are here to
# make possible. Decide before phase 3 lands, not after.
#
# Idempotent. Safe to re-run. Requires a superuser connection.
#
#   PGPASSWORD=... ./scripts/init-schema-roles.sh
#
set -euo pipefail

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
PGDATABASE="${PGDATABASE:-telemed}"
: "${TELEMED_DB_ROLE_PASSWORD:?TELEMED_DB_ROLE_PASSWORD is required (scripts/gen-secrets.sh)}"

DOMAINS=(user doctor scheduling consultation payment notification record admin)

psql_run() { psql -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" "$@"; }

echo "==> revoking the default public grants"
psql_run <<'SQL'
-- PUBLIC can CREATE in the public schema on PostgreSQL below 15, and can
-- always CONNECT to the database. Neither is wanted.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database())
\gexec
SQL

for domain in "${DOMAINS[@]}"; do
  schema="svc_${domain}"
  mig="telemed_${domain}_mig"
  app="telemed_${domain}_app"
  echo "==> ${schema}: ${mig} (owner) / ${app} (dml)"

  psql_run \
    -v schema="$schema" -v mig="$mig" -v app="$app" \
    -v role_pw="$TELEMED_DB_ROLE_PASSWORD" <<'SQL'
-- Roles first. \gexec makes CREATE ROLE idempotent without a DO block.
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'mig', :'role_pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'mig')
\gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'app', :'role_pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app')
\gexec

-- Both roles need to reach the database at all.
SELECT format('GRANT CONNECT, TEMPORARY ON DATABASE %I TO %I', current_database(), :'mig')
\gexec
SELECT format('GRANT CONNECT, TEMPORARY ON DATABASE %I TO %I', current_database(), :'app')
\gexec

-- The schema itself. REVOKE FROM PUBLIC is the load-bearing line: without it
-- every role on the server has USAGE here by default, and the whole point of
-- separating the domains by schema is gone.
SELECT format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', :'schema') \gexec
SELECT format('ALTER SCHEMA %I OWNER TO %I', :'schema', :'mig') \gexec
SELECT format('GRANT USAGE, CREATE ON SCHEMA %I TO %I', :'schema', :'mig') \gexec
SELECT format('GRANT USAGE ON SCHEMA %I TO %I', :'schema', :'app') \gexec

-- Shared extensions live in public; the app roles read from it, never write.
SELECT format('GRANT USAGE ON SCHEMA public TO %I', :'app') \gexec
SELECT format('GRANT USAGE ON SCHEMA public TO %I', :'mig') \gexec

-- DML on everything the mig role creates from here on. Deliberately DEFAULT
-- PRIVILEGES rather than a blanket grant over existing tables: the migrations
-- already GRANT per table where they want something narrower (audit_logs is
-- append-only, and REVOKE UPDATE there must not be undone by a later blanket
-- GRANT). FOR ROLE ties the default to the creating role, which is the only
-- role that ever creates in this schema.
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I',
  :'mig', :'schema', :'app') \gexec
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO %I',
  :'mig', :'schema', :'app') \gexec
SQL
done

echo
echo "==> proving the boundary holds"
fail=0
for a in "${DOMAINS[@]}"; do
  for b in "${DOMAINS[@]}"; do
    [[ "$a" == "$b" ]] && continue
    got=$(psql_run -tAc "SELECT has_schema_privilege('telemed_${a}_app', 'svc_${b}', 'USAGE')")
    if [[ "$got" != "f" ]]; then
      echo "  FAIL telemed_${a}_app has USAGE on svc_${b} -- the domains are not isolated"
      fail=1
    fi
  done
  owned=$(psql_run -tAc "
    SELECT count(*) FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'svc_${a}' AND c.relowner = 'telemed_${a}_app'::regrole")
  if [[ "$owned" != "0" ]]; then
    echo "  FAIL telemed_${a}_app owns ${owned} relation(s) in svc_${a}; an owner can DROP, ALTER and re-GRANT"
    fail=1
  fi
done
if [[ "$fail" == "0" ]]; then
  echo "  ok  every app role reaches its own schema and no other, and owns nothing"
else
  exit 1
fi
