# identity: build from the repository root,
#   docker build -f deploy/docker/identity.Dockerfile .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/identity ./cmd/identity

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY --from=build /out/identity /usr/local/bin/identity
USER 10001
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/identity"]
