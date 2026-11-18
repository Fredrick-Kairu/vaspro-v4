# contacts: build from the repository root,
#   docker build -f deploy/docker/contacts.Dockerfile .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/contacts ./cmd/contacts

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY packs/ packs/
COPY --from=build /out/contacts /usr/local/bin/contacts
USER 10001
EXPOSE 8086
EXPOSE 9093
ENTRYPOINT ["/usr/local/bin/contacts"]
