#!/bin/sh
# Creates deploy/compose/.env for the dev environment from env.example,
# filling every empty secret with fresh random values, and prints the
# Mailpit basic-auth password once (only its bcrypt hash is stored).
# Never overwrites an existing .env. Needs openssl and Docker.
# See docs/runbooks/dev-environment.md.
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
env_file="$dir/.env"
if [ -e "$env_file" ]; then
	echo "dev-secrets: $env_file already exists; leaving it alone" >&2
	exit 1
fi

key32() { openssl rand -base64 32; }  # LocalKMS and HMAC keys: 32 bytes
password() { openssl rand -hex 24; }  # URL-safe: they go in connection URLs

mail_password=$(openssl rand -hex 12)
mail_hash=$(docker run --rm caddy:2 caddy hash-password --plaintext "$mail_password")

umask 077
tmp="$env_file.tmp"
while IFS= read -r line; do
	case "$line" in
	*_PASSWORD=) [ "${line#POSTGRES_}" = "$line" ] && line="$line$(password)" ;;
	IDENTITY_TOTP_KMS_KEY= | IDENTITY_SIGNING_KEYS_KMS_KEY= | IDENTITY_CHALLENGE_SIGNING_KEY= | ACCOUNTS_DOCUMENTS_KMS_KEY= | ACCOUNTS_KYC_WEBHOOK_SECRET=)
		line="$line$(key32)" ;;
	MAIL_BASIC_AUTH_HASH=) line="$line'$mail_hash'" ;;
	esac
	printf '%s\n' "$line"
done <"$dir/env.example" >"$tmp"
mv "$tmp" "$env_file"
echo "dev-secrets: wrote $env_file"
echo "dev-secrets: Mailpit's password (shown once, not stored): $mail_password"
