FROM golang:1.24-alpine AS builder

# Install build dependencies for CGO (needed for SQLite)
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o pr-review-server .

# Final stage
FROM alpine:3.20

# Install required packages
RUN apk --no-cache add \
    ca-certificates \
    sqlite-libs \
    bash \
    wget \
    espeak-ng \
    alsa-utils \
    pulseaudio-utils

WORKDIR /app

# Copy the Go binary from builder
COPY --from=builder /app/pr-review-server .

# Copy cbpr binary (either real Linux binary or placeholder)
# To use real cbpr: run ./build-cbpr-linux.sh before docker build
COPY bin/ /tmp/bin/
RUN if [ -f /tmp/bin/cbpr-linux ]; then \
        mv /tmp/bin/cbpr-linux /usr/local/bin/cbpr && \
        chmod +x /usr/local/bin/cbpr && \
        echo "✓ cbpr binary installed"; \
    else \
        mv /tmp/bin/cbpr-placeholder.sh /usr/local/bin/cbpr && \
        chmod +x /usr/local/bin/cbpr && \
        echo "⚠ cbpr placeholder installed - run ./build-cbpr-linux.sh to enable reviews"; \
    fi && rm -rf /tmp/bin

# Create directories for data
RUN mkdir -p /app/reviews /app/data

# Create non-root user for security
RUN addgroup -S appgroup && adduser -S -h /home/appuser appuser -G appgroup && \
    chown -R appuser:appgroup /app

# Switch to non-root user
USER appuser

# Set home directory to allow tools like git to find user-level config
ENV HOME=/home/appuser

# Volume mounts for persistence
VOLUME ["/app/reviews", "/app/data"]

# Expose web server port
EXPOSE 8080

# Run the server
CMD ["./pr-review-server"]
