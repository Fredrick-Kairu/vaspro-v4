#!/bin/sh
# Creates deploy/compose/.env for the dev environment from env.example,
# filling every empty secret with fresh random values. Never overwrites
# an existing .env. See docs/runbooks/dev-environment.md.
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
env_file="$dir/.env"
if [ -e "$env_file" ]; then
	echo "dev-secrets: $env_file already exists; leaving it alone" >&2
	exit 1
fi

key32() { openssl rand -base64 32; }  # LocalKMS and HMAC keys: 32 bytes
password() { openssl rand -hex 24; }  # URL-safe: they go in connection URLs

umask 077
while IFS= read -r line; do
	case "$line" in
	*_PASSWORD=) [ "${line#POSTGRES_}" = "$line" ] && line="$line$(password)" ;;
	IDENTITY_TOTP_KMS_KEY= | IDENTITY_SIGNING_KEYS_KMS_KEY= | IDENTITY_CHALLENGE_SIGNING_KEY= | ACCOUNTS_DOCUMENTS_KMS_KEY= | ACCOUNTS_KYC_WEBHOOK_SECRET=)
		line="$line$(key32)" ;;
	esac
	printf '%s\n' "$line"
done <"$dir/env.example" >"$env_file"
echo "dev-secrets: wrote $env_file"
