# Fetch
FROM golang:latest AS fetch-stage
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# Build
FROM golang:1.23.0 AS build-stage
WORKDIR /app
COPY --from=fetch-stage /app/go.mod /app/go.sum ./
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -o /app/entrypoint

# Test
FROM build-stage AS test-stage
RUN go test -v ./...

# Deploy
FROM gcr.io/distroless/base-debian12 AS deploy-stage
WORKDIR /
COPY --from=build-stage /app/entrypoint /entrypoint
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/entrypoint"]
