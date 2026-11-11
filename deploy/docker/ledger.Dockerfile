# ledger: build from the repository root,
#   docker build -f deploy/docker/ledger.Dockerfile .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledger ./cmd/ledger

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY packs/ packs/
COPY --from=build /out/ledger /usr/local/bin/ledger
USER 10001
EXPOSE 8083 9092
ENTRYPOINT ["/usr/local/bin/ledger"]
