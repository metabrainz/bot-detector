# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download
RUN go mod tidy

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
