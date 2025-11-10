# --- STAGE 1: Build the Go application ---
FROM golang:1.22-alpine AS builder

# Set necessary environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux

# Create and set the working directory inside the container
WORKDIR /app

# Copy the Go application source code
COPY main.go .
# Copy the dependency file (assuming config.yaml is needed for final image)
COPY config.yaml .

# Fetch Go dependencies (Go 1.16+ handles this automatically if no go.mod/go.sum, 
# but including a placeholder for future dependency management)

# Initialize go modules and download dependencies
RUN go mod init bot-detector
RUN go mod tidy

# Build the Go application
# Use the -a flag to ensure a statically linked binary for the final Alpine image
RUN go build -a -ldflags '-s -w' -o bot-detector main.go


# --- STAGE 2: Create the final minimal image ---
FROM alpine:latest

# Security best practice: use a non-root user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

# Set the working directory
WORKDIR /home/appuser/bot-detector

# Copy the built binary from the builder stage
COPY --from=builder /app/bot-detector .

# Copy the configuration file (which will be mounted over by the host)
COPY --from=builder /app/config.yaml .

# Set default command if user runs without arguments (helpful for debugging/info)
ENTRYPOINT ["./bot-detector"]
CMD ["-h"]

