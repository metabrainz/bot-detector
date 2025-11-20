# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

# Create a non-root user for security in the builder stage as well
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app
# Change ownership of the working directory to appuser
RUN chown appuser:appgroup /app

# Copy Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download
RUN go mod tidy

# Copy the rest of the source code with proper ownership
COPY --chown=appuser:appgroup . .

# Switch to non-root user for the build
USER appuser

# Build the Go application for a static binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot-detector ./cmd/bot-detector

# Stage 2: Create the final minimal image
FROM alpine:latest

# Create a non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /home/appuser/bot-detector

# Create required directories and ensure correct ownership
RUN mkdir -p /home/appuser/bot-detector/config \
    && mkdir -p /home/appuser/bot-detector/config.backup \
    && mkdir -p /home/appuser/bot-detector/state \
    && chown -R appuser:appgroup /home/appuser/bot-detector

# Copy the built binary from the builder stage
COPY --from=builder /app/bot-detector .

# Drop privileges
USER appuser

# Default command to show help
ENTRYPOINT ["./bot-detector"]
CMD ["-h"]
