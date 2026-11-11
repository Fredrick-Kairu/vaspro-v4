# migrate: the dev environment's one-shot database set-up
# (deploy/compose/db-setup.sh): roles, goose migrations, grants.
#   docker build -f deploy/docker/migrate.Dockerfile .
FROM golang:1.26.11-alpine AS build
# goose's version matches go.mod's; the tags leave out drivers we don't use.
RUN CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" \
    -tags='no_clickhouse no_libsql no_mssql no_mysql no_sqlite3 no_vertica no_ydb' \
    github.com/pressly/goose/v3/cmd/goose@v3.29.0

FROM alpine:3.22
RUN apk add --no-cache postgresql16-client && adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY --from=build /go/bin/goose /usr/local/bin/goose
COPY migrations/ migrations/
COPY deploy/compose/db-setup.sh /usr/local/bin/db-setup
USER 10001
ENTRYPOINT ["/usr/local/bin/db-setup"]
