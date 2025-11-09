# Example Dockerfile

This document provides a recommended multi-stage `Dockerfile` for building a minimal, production-ready container image for the bot-detector.

## Multi-Stage Dockerfile

Using a multi-stage build allows you to compile the application in a temporary build environment with all the necessary Go tooling, and then copy only the final compiled binary into a lightweight, clean final image. This results in a smaller and more secure container.

```dockerfile
# Dockerfile

# --- Build Stage ---
# Use the official Go image as the build environment.
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy Go module files and download dependencies first to leverage Docker layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code.
COPY . .

# Build the application, creating a statically linked binary.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot-detector .

# --- Final Stage ---
# Use a minimal base image for the final container.
FROM alpine:latest

WORKDIR /home/appuser/bot-detector
COPY --from=builder /app/bot-detector .

# (Optional) Copy default configuration if you want it baked into the image.
# COPY config.yaml .

# The entrypoint is the application itself. Flags will be passed to it.
ENTRYPOINT ["./bot-detector"]
```