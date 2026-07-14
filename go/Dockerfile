# Multi-target build: docker compose picks one binary per service via `target:`.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.20 AS base
RUN apk add --no-cache ca-certificates

FROM base AS ingest
COPY --from=build /out/ingest /ingest
EXPOSE 8081
ENTRYPOINT ["/ingest"]

FROM base AS dispatch
COPY --from=build /out/dispatch /dispatch
EXPOSE 8082
ENTRYPOINT ["/dispatch"]

FROM base AS receiver
COPY --from=build /out/receiver /receiver
EXPOSE 8090
ENTRYPOINT ["/receiver"]
