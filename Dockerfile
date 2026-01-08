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
FROM alpine:latest

# Install required packages
RUN apk --no-cache add \
    ca-certificates \
    sqlite-libs \
    bash

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/pr-review-server .

# Create directories for data
RUN mkdir -p /app/reviews /app/data

# Volume mounts for persistence
VOLUME ["/app/reviews", "/app/data"]

# Expose web server port
EXPOSE 8080

# Run the server
CMD ["./pr-review-server"]
