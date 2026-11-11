# kycvendor: build from the repository root,
#   docker build -f deploy/docker/kycvendor.Dockerfile .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kycvendor ./tools/fakes/kycvendor/cmd/kycvendor

FROM alpine:3.22
RUN adduser -D -H -u 10001 vaspro
WORKDIR /app
COPY --from=build /out/kycvendor /usr/local/bin/kycvendor
USER 10001
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/kycvendor"]
CMD ["serve"]
