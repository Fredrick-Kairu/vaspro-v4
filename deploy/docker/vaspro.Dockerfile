# vaspro, the operations CLI (staff grants, signing-key rotation,
# pack validation): build from the repository root,
#   docker build -f deploy/docker/vaspro.Dockerfile .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vaspro ./tools/vaspro

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY packs/ packs/
COPY --from=build /out/vaspro /usr/local/bin/vaspro
USER 10001
ENTRYPOINT ["/usr/local/bin/vaspro"]
