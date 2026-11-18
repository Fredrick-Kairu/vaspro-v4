#!/bin/sh
# Renders vaspro-dev.conf.template for the host's nginx:
#
#   deploy/nginx/render.sh <api-domain> <mail-domain> [upload-origins] > vaspro-dev.conf
#
# upload-origins: the browser origins (the dashboard's) that may upload
# contacts' import files, comma-separated, such as
# https://app.example.com,http://localhost:5173; none means browsers
# can't read the answer to an upload.
#
# The ports come from GATEWAY_HOST_PORT, MAILPIT_HOST_PORT and
# OBJECT_STORAGE_HOST_PORT (defaults 18080, 18025 and 18333, as in
# deploy/compose/env.example); keep them equal to the dev stack's. Only
# these placeholders are replaced, so nginx's own $variables are left
# alone.
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
	echo "usage: $0 <api-domain> <mail-domain> [upload-origins]" >&2
	exit 2
fi
api=$1
mail=$2
origins=${3:-}
gateway_port=${GATEWAY_HOST_PORT:-18080}
mailpit_port=${MAILPIT_HOST_PORT:-18025}
storage_port=${OBJECT_STORAGE_HOST_PORT:-18333}

for name in "$api" "$mail"; do
	case "$name" in
	*[!A-Za-z0-9.-]* | "" | .* | *..*)
		echo "render: '$name' isn't a host name" >&2
		exit 1
		;;
	esac
done
for port in "$gateway_port" "$mailpit_port" "$storage_port"; do
	case "$port" in
	*[!0-9]* | "")
		echo "render: '$port' isn't a port" >&2
		exit 1
		;;
	esac
done

# The origins become one regular expression, dots escaped; none matches
# nothing (a pattern no Origin header can be).
pattern='no-origin\\.invalid'
if [ -n "$origins" ]; then
	pattern=""
	for o in $(echo "$origins" | tr ',' ' '); do
		case "$o" in
		http://*.* | https://*.* | http://localhost* | https://localhost*) ;;
		*)
			echo "render: '$o' isn't an origin like https://app.example.com" >&2
			exit 1
			;;
		esac
		case "$o" in
		*[!A-Za-z0-9.:/-]* | */*/*/*)
			echo "render: '$o' isn't an origin like https://app.example.com" >&2
			exit 1
			;;
		esac
		esc=$(printf '%s' "$o" | sed 's/\./\\\\./g')
		pattern="${pattern:+$pattern|}$esc"
	done
fi

sed -e "s/\${API_DOMAIN}/$api/g" \
	-e "s/\${MAIL_DOMAIN}/$mail/g" \
	-e "s/\${GATEWAY_HOST_PORT}/$gateway_port/g" \
	-e "s/\${MAILPIT_HOST_PORT}/$mailpit_port/g" \
	-e "s/\${OBJECT_STORAGE_HOST_PORT}/$storage_port/g" \
	-e "s#\${UPLOAD_ORIGINS}#$pattern#g" \
	"$(dirname "$0")/vaspro-dev.conf.template"
