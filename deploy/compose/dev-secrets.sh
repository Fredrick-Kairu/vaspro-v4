#!/bin/sh
# Creates deploy/compose/.env for the dev environment from env.example,
# filling every empty secret with fresh random values. Never overwrites
# an existing .env. Needs openssl.
#
#   deploy/compose/dev-secrets.sh                # a new .env
#   deploy/compose/dev-secrets.sh --add-missing  # append settings a newer
#                                                # env.example added
#
# --add-missing appends only the variables .env doesn't have yet, with
# new secrets for them, and never changes a line already there.
# See docs/runbooks/dev-environment.md.
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
env_file="$dir/.env"
add_missing=false
case "${1:-}" in
"") ;;
--add-missing) add_missing=true ;;
*)
	echo "usage: dev-secrets.sh [--add-missing]" >&2
	exit 2
	;;
esac
if [ -e "$env_file" ] && ! $add_missing; then
	echo "dev-secrets: $env_file already exists; leaving it alone (--add-missing appends new settings)" >&2
	exit 1
fi
if [ ! -e "$env_file" ] && $add_missing; then
	echo "dev-secrets: $env_file doesn't exist yet; run without --add-missing" >&2
	exit 1
fi

key32() { openssl rand -base64 32; }  # LocalKMS and HMAC keys: 32 bytes
password() { openssl rand -hex 24; }  # URL-safe: they go in connection URLs

# fill prints an env.example line with its secret filled in.
fill() {
	line=$1
	case "$line" in
	*_PASSWORD=) [ "${line#POSTGRES_}" = "$line" ] && line="$line$(password)" ;;
	IDENTITY_TOTP_KMS_KEY= | IDENTITY_SIGNING_KEYS_KMS_KEY= | IDENTITY_CHALLENGE_SIGNING_KEY= | ACCOUNTS_DOCUMENTS_KMS_KEY= | ACCOUNTS_KYC_WEBHOOK_SECRET= | \
		PAYMENTS_CREDENTIALS_KMS_KEY= | PAYMENTS_CALLBACK_SECRET= | PAYMENTS_CHECKOUT_SECRET= | PAYMENTS_CREDENTIAL_FINGERPRINT_KEY=)
		line="$line$(key32)" ;;
	esac
	printf '%s\n' "$line"
}

umask 077
if $add_missing; then
	added=""
	while IFS= read -r line; do
		case "$line" in
		[A-Z]*=*) ;;
		*) continue ;;
		esac
		name=${line%%=*}
		grep -q "^$name=" "$env_file" && continue
		fill "$line" >>"$env_file"
		added="$added $name"
	done <"$dir/env.example"
	if [ -z "$added" ]; then
		echo "dev-secrets: $env_file already has every setting"
	else
		# Names only: the values are secrets.
		echo "dev-secrets: added to $env_file:$added"
	fi
	exit 0
fi

tmp="$env_file.tmp"
while IFS= read -r line; do
	fill "$line"
done <"$dir/env.example" >"$tmp"
mv "$tmp" "$env_file"
echo "dev-secrets: wrote $env_file"
