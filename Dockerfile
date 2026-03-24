# Stage 1: Build React frontend
FROM node:20-alpine AS frontend-builder

WORKDIR /app/frontend

# Copy frontend package files
COPY frontend/package*.json ./
RUN npm ci

# Copy frontend source
COPY frontend/ ./

# Build React app (outputs to ../server/dist)
RUN npm run build

# Stage 2: Build Go backend
FROM golang:1.24-alpine AS backend-builder

# Install build dependencies for CGO (needed for SQLite)
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Copy built frontend from previous stage
COPY --from=frontend-builder /app/server/dist ./server/dist

# Build the application with embedded React app
# Use -ldflags="-w -s" to strip debug info and reduce binary size
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o pr-review-server .

# Stage 3: Final runtime image
FROM alpine:3.20

# Install required packages
RUN apk --no-cache add \
    ca-certificates \
    sqlite-libs \
    bash \
    wget

WORKDIR /app

# Copy the Go binary from builder
COPY --from=backend-builder /app/pr-review-server .

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
