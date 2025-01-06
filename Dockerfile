# Fetch
FROM golang:latest AS fetch-stage
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# Build
FROM golang:1.23.4 AS build-stage
WORKDIR /app
COPY --from=fetch-stage /app/go.mod /app/go.sum ./
COPY main.go .
RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -o /app/entrypoint

# Test
FROM build-stage AS test-stage
RUN go test -v ./...

# Deploy
FROM debian:bookworm-slim AS deploy-stage
RUN apt-get update && apt-get install -y \
    ca-certificates \
    && update-ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /
COPY --from=build-stage /app/entrypoint /entrypoint
RUN mkdir /data && chown nobody:nogroup /data
EXPOSE 8080
USER nobody:nogroup
ENTRYPOINT ["/entrypoint"]
