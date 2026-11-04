#!/bin/sh
# Dev environment database set-up (docs/runbooks/dev-environment.md).
# Runs in the migrate image as the PostgreSQL superuser (PGHOST, PGUSER,
# PGPASSWORD, PGDATABASE) and is safe to run again:
#
#   1. extensions the migrations need, created once as the superuser;
#   2. per service, <svc>_owner (runs its migrations and owns its schema)
#      and <svc>_app (the service's login role, which owns nothing),
#      both with search_path <svc>,public. The owner has CREATEROLE (the
#      migrations create their narrow roles) and BYPASSRLS: migrations
#      seed rows no tenant may write, such as accounts' built-in roles,
#      which the tests do as the superuser. The app role has neither,
#      so RLS binds the running service;
#   3. goose migrations per schema as the owner, each service with its
#      own version table in the goose schema;
#   4. the app roles' grants, including the narrow roles each service
#      reaches only through SET ROLE (WITH INHERIT FALSE).
#
# Passwords come from <SVC>_OWNER_PASSWORD and <SVC>_DB_PASSWORD and are
# never printed.
set -eu

SERVICES="identity accounts ledger"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-/app/migrations}"

for svc in $SERVICES; do
	up=$(echo "$svc" | tr '[:lower:]' '[:upper:]')
	for var in "${up}_OWNER_PASSWORD" "${up}_DB_PASSWORD"; do
		eval "val=\${$var:-}"
		if [ -z "$val" ]; then
			echo "db-setup: $var is required" >&2
			exit 1
		fi
	done
done

psql_super() {
	psql -X -q -v ON_ERROR_STOP=1 "$@"
}

echo "db-setup: extensions and the goose schema"
psql_super <<'SQL'
CREATE EXTENSION IF NOT EXISTS citext SCHEMA public;
CREATE EXTENSION IF NOT EXISTS btree_gist SCHEMA public;
CREATE SCHEMA IF NOT EXISTS goose;
SQL

for svc in $SERVICES; do
	up=$(echo "$svc" | tr '[:lower:]' '[:upper:]')
	echo "db-setup: $svc roles"
	# \getenv keeps the passwords off the command line.
	psql_super -v svc="$svc" <<SQL
\getenv owner_pw ${up}_OWNER_PASSWORD
\getenv app_pw ${up}_DB_PASSWORD
SELECT format('CREATE ROLE %I LOGIN CREATEROLE', :'svc' || '_owner')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'svc' || '_owner') \gexec
SELECT format('CREATE ROLE %I LOGIN', :'svc' || '_app')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'svc' || '_app') \gexec
SELECT format('ALTER ROLE %I CREATEROLE BYPASSRLS PASSWORD %L', :'svc' || '_owner', :'owner_pw') \gexec
SELECT format('ALTER ROLE %I PASSWORD %L', :'svc' || '_app', :'app_pw') \gexec
SELECT format('ALTER ROLE %I SET search_path = %I, public', r, :'svc')
FROM unnest(ARRAY[:'svc' || '_owner', :'svc' || '_app']) AS r \gexec
SELECT format('GRANT CREATE ON DATABASE %I TO %I', current_database(), :'svc' || '_owner') \gexec
SELECT format('GRANT USAGE, CREATE ON SCHEMA goose TO %I', :'svc' || '_owner') \gexec
SQL

	echo "db-setup: $svc migrations"
	eval "owner_pw=\${${up}_OWNER_PASSWORD}"
	PGUSER="${svc}_owner" PGPASSWORD="$owner_pw" \
		goose -dir "$MIGRATIONS_DIR/$svc" -table "goose.${svc}_version" \
		postgres "sslmode=${PGSSLMODE:-disable}" up
done

echo "db-setup: grants"
for svc in identity accounts; do
	psql_super -v svc="$svc" <<'SQL'
SELECT format('GRANT USAGE ON SCHEMA %I TO %I', :'svc', :'svc' || '_app') \gexec
SELECT format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO %I', :'svc', :'svc' || '_app') \gexec
SELECT format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO %I', :'svc', :'svc' || '_app') \gexec
SELECT format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I', :'svc' || '_owner', :'svc', :'svc' || '_app') \gexec
SELECT format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO %I', :'svc' || '_owner', :'svc', :'svc' || '_app') \gexec
SQL
done

psql_super <<'SQL'
-- Narrow roles, reached only inside db.Pool.WithRoleTx.
GRANT identity_token_exchange      TO identity_app WITH INHERIT FALSE;
GRANT identity_sso_domain_lookup   TO identity_app WITH INHERIT FALSE;
GRANT accounts_membership_lookup   TO accounts_app WITH INHERIT FALSE;
GRANT accounts_kyc_webhook         TO accounts_app WITH INHERIT FALSE;
GRANT accounts_registration_lookup TO accounts_app WITH INHERIT FALSE;
GRANT accounts_staff_review        TO accounts_app WITH INHERIT FALSE;
-- The ledger's grant role (task 3.1): its migrations grant it exactly
-- what the service may do, and it reaches ledger_system,
-- ledger_price_review and ledger_settlement itself (WITH INHERIT FALSE).
GRANT ledger_service TO ledger_app;
SQL

echo "db-setup: done"
