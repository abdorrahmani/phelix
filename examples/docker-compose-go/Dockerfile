# Stage 1: Build
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /app

# CA bundle for the scratch runtime (any outbound HTTPS needs it)
RUN apk add --no-cache ca-certificates

# Copy dependency manifests first for layer caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server .

# Stage 2: Runtime (minimal image)
FROM scratch

# CA certificates so the app can make outbound HTTPS calls
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /app/server /server

# Run as a non-root user (numeric — scratch has no /etc/passwd)
USER 65532:65532

ENTRYPOINT ["/server"]
