# gateway: build from the repository root,
#   docker build -f deploy/docker/gateway.Dockerfile .
FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY docs/api/*.yaml docs/api/
COPY --from=build /out/gateway /usr/local/bin/gateway
USER 10001
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/gateway"]
