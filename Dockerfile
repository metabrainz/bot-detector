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

# Switch to non-root user after go mod commands
USER appuser

# Copy the rest of the source code
COPY . .

# Build the Go application for a static binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot-detector .

# Stage 2: Create the final minimal image
FROM alpine:latest

# Create a non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

WORKDIR /home/appuser/bot-detector

# Copy the built binary from the builder stage
COPY --from=builder /app/bot-detector .

# Default command to show help
ENTRYPOINT ["./bot-detector"]
CMD ["-h"]