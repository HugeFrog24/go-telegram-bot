# Multi-stage build for Go Telegram Bot
# Build stage
FROM golang:1.24-alpine AS builder

# Install build dependencies including C compiler for CGO
RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev

# Set working directory
WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o telegram-bot .

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates tzdata sqlite

# Create non-root user
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Set working directory
WORKDIR /app

# Create necessary directories
RUN mkdir -p /app/config /app/data /app/logs && \
    chown -R appuser:appgroup /app

# Copy binary from builder stage
COPY --from=builder /build/telegram-bot /app/telegram-bot

# Copy default config as template
COPY --chown=appuser:appgroup config/default.json /app/config/

# Switch to non-root user
USER appuser

# Expose any ports if needed (not required for this bot)
# EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD pgrep telegram-bot || exit 1

# Run the application
CMD ["/app/telegram-bot"]