#!/usr/bin/env bash
#
# Apply the consolidated migrations: bootstrap once, then each domain into its
# own schema.
#
# Before consolidation this was nine `migrate -path migrations -database
# $DATABASE_URL up` invocations, one per repository, each against its own
# database. Now it is one database and one repository, and the thing that keeps
# a domain's tables out of another domain's way is search_path -- passed as a
# connection parameter, so it applies to every connection golang-migrate opens
# rather than only the one a `SET` happened to run on.
#
# Each domain keeps its OWN schema_migrations table, inside its own schema.
# That is deliberate: the eight migration histories were independent before and
# stay independent, so a domain can be rolled back without touching the others.
#
#   DATABASE_URL=postgres://... ./scripts/migrate.sh            # all domains
#   DATABASE_URL=postgres://... ./scripts/migrate.sh user admin # just these
#   MIGRATE_DIRECTION=down ./scripts/migrate.sh user
#
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
DIRECTION="${MIGRATE_DIRECTION:-up}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ALL_DOMAINS=(user doctor scheduling consultation payment notification record admin)
DOMAINS=("${@:-}")
if [[ -z "${DOMAINS[*]}" ]]; then
  DOMAINS=("${ALL_DOMAINS[@]}")
fi

command -v migrate >/dev/null || {
  echo "migrate CLI not found. Install with:" >&2
  echo "  go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1" >&2
  exit 1
}

# Append a query parameter to a URL-form DSN.
with_param() {
  local url="$1" param="$2"
  if [[ "$url" == *"?"* ]]; then echo "${url}&${param}"; else echo "${url}?${param}"; fi
}

if [[ "$DIRECTION" == "up" ]]; then
  echo "==> bootstrap: schemas and shared extensions"
  # No search_path: this migration creates the schemas the others need, and
  # runs against the database default.
  migrate -path "${ROOT}/migrations/bootstrap" -database "$DATABASE_URL" up
fi

for domain in "${DOMAINS[@]}"; do
  schema="svc_${domain}"
  dir="${ROOT}/migrations/${domain}"
  [[ -d "$dir" ]] || { echo "no migrations for domain '${domain}'" >&2; exit 1; }

  # x-migrations-table is golang-migrate's own parameter and is consumed by it;
  # search_path is passed through to the server. Both are needed: without
  # search_path the DDL lands in public, and without the explicit migrations
  # table the eight histories would collide on one schema_migrations row.
  dsn="$(with_param "$DATABASE_URL" "search_path=${schema},public")"
  dsn="$(with_param "$dsn" "x-migrations-table=schema_migrations")"

  echo "==> ${domain} -> ${schema} (${DIRECTION})"
  if [[ "$DIRECTION" == "down" ]]; then
    migrate -path "$dir" -database "$dsn" down -all
  else
    # Automatically clear dirty state if a previous migration run failed midway
    v=$(migrate -path "$dir" -database "$dsn" version 2>&1 || true)
    if [[ "$v" == *"(dirty)"* ]]; then
      dirty_ver=$(echo "$v" | awk '{print $1}')
      prev_ver=$((dirty_ver - 1))
      if (( prev_ver < 0 )); then prev_ver=0; fi
      echo "==> ${domain} is dirty at version ${dirty_ver}; clearing dirty state and forcing to ${prev_ver}"
      migrate -path "$dir" -database "$dsn" force "$prev_ver"
    fi
    migrate -path "$dir" -database "$dsn" up
  fi
done

if [[ "$DIRECTION" == "down" ]]; then
  echo "==> bootstrap: dropping schemas"
  migrate -path "${ROOT}/migrations/bootstrap" -database "$DATABASE_URL" down -all
fi

echo "done."
