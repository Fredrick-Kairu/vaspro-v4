#!/bin/sh
# Renders vaspro-dev.conf.template for the host's nginx:
#
#   deploy/nginx/render.sh <api-domain> <mail-domain> > vaspro-dev.conf
#
# The ports come from GATEWAY_HOST_PORT and MAILPIT_HOST_PORT (defaults
# 18080 and 18025, as in deploy/compose/env.example); keep them equal to
# the dev stack's. Only these four placeholders are replaced, so nginx's
# own $variables are left alone.
set -eu

if [ $# -ne 2 ]; then
	echo "usage: $0 <api-domain> <mail-domain>" >&2
	exit 2
fi
api=$1
mail=$2
gateway_port=${GATEWAY_HOST_PORT:-18080}
mailpit_port=${MAILPIT_HOST_PORT:-18025}

for name in "$api" "$mail"; do
	case "$name" in
	*[!A-Za-z0-9.-]* | "" | .* | *..*)
		echo "render: '$name' isn't a host name" >&2
		exit 1
		;;
	esac
done
for port in "$gateway_port" "$mailpit_port"; do
	case "$port" in
	*[!0-9]* | "")
		echo "render: '$port' isn't a port" >&2
		exit 1
		;;
	esac
done

sed -e "s/\${API_DOMAIN}/$api/g" \
	-e "s/\${MAIL_DOMAIN}/$mail/g" \
	-e "s/\${GATEWAY_HOST_PORT}/$gateway_port/g" \
	-e "s/\${MAILPIT_HOST_PORT}/$mailpit_port/g" \
	"$(dirname "$0")/vaspro-dev.conf.template"
