# Running with Docker

This guide provides instructions for building and running the bot-detector in a Docker container for production environments.

## Building the Docker Image

A multi-stage [`Dockerfile`](../Dockerfile) is provided at the root of this project. It compiles the application in a build container and then copies the static binary to a minimal `alpine` image for a small and secure result.

### Using the Build Script (Recommended)

The easiest way to build the image is to use the provided [`docker-build.sh`](../docker-build.sh) script:

```sh
./docker-build.sh
```

#### What the Script Does

The build script automates the Docker build process and handles version tagging:

1. **Captures git commit** - Retrieves the current short commit hash (or "unknown" if not in a git repository)
2. **Builds the image** - Executes `docker build` with the Dockerfile
3. **Embeds version info** - Go 1.18+ automatically embeds VCS information when the `.git` directory is present
4. **Creates dual tags** - Tags the image as both:
   - `bot-detector:latest` (for convenience)
   - `bot-detector:<commit>` (for version tracking)

This ensures that every build is traceable to a specific commit, which is essential for production deployments and debugging.

### Manual Build

Alternatively, you can build manually:

```sh
docker build -t bot-detector:latest .
```

Note: Manual builds will show `commit: unknown` and `built: unknown` in the version output.

### Verifying the Build

After building, you can verify the version information:

```sh
docker run --rm --entrypoint bot-detector bot-detector:latest --version
```

Example output:
```
bot-detector version 0.1 (commit: ebaa0a5, built: 2025-11-21_22:34:05)
```

For a running container:
```sh
docker exec <container-name> bot-detector --version
```

## Running the Container

The recommended way to run the container is to use environment variables to define your paths and ports, which makes the command easier to read and manage.

### Live Mode Example

This example demonstrates a robust setup for a production environment.

**1. Set up your environment variables:**
```sh
# --- Configuration ---
# The name for this specific instance (e.g., for a specific log file)
INSTANCE_NAME="prod-website"
# The directory on the HOST machine where your `config.yaml` and its dependencies are.
HOST_CONFIG_DIR="/etc/bot-detector/${INSTANCE_NAME}"
# The directory on the HOST machine where the logs to be tailed are.
HOST_LOGS_DIR="/var/log/nginx/"
# The specific log file to tail inside the HOST_LOGS_DIR.
LOG_FILE_NAME="access.log"
# The directory on the HOST machine where the application's state will be persisted.
HOST_STATE_DIR="/var/lib/bot-detector/${INSTANCE_NAME}"
# The port on the HOST machine to expose the API server on.
BOTDET_API_PORT=8090

# --- Docker Settings ---
CONTAINER_NAME="bot-detector-${INSTANCE_NAME}"
IMAGE_NAME="bot-detector:latest"
# The internal port the app listens on. This should match the --http-server flag.
INTERNAL_PORT=8088
```

**2. Create the state directory on the host:**
The application needs a directory to store its state. You must create it on the host machine before running the container.
```sh
mkdir -p "$HOST_STATE_DIR"
```

**3. Run the `docker run` command:**
This command stops and removes any old container with the same name before starting a new one.
```sh
# Stop and remove any existing container
docker rm -f "$CONTAINER_NAME"

# Run the new container
docker run -d \
  --name "$CONTAINER_NAME" \
  --restart always \
  -v "$HOST_CONFIG_DIR":/home/appuser/bot-detector/config:ro \
  -v "$HOST_LOGS_DIR":/home/appuser/bot-detector/logs:ro \
  -v "$HOST_STATE_DIR":/home/appuser/bot-detector/state \
  --publish $BOTDET_API_PORT:$INTERNAL_PORT \
  "$IMAGE_NAME" \
  --config-dir "config" \
  --log-path "logs/$LOG_FILE_NAME" \
  --state-dir "state" \
  --http-server "0.0.0.0:$INTERNAL_PORT"
```

### Explanation of the docker run Command

*   `-d`: Runs the container in detached mode (in the background).
*   `--name "$CONTAINER_NAME"`: Assigns a predictable name to the container.
*   `--restart always`: Ensures the container will restart automatically if it stops.
*   `-v "$HOST_CONFIG_DIR":/home/appuser/bot-detector/config:ro`: Mounts your configuration directory from the host into the container in **read-only** mode for security.
*   `-v "$HOST_LOGS_DIR":/home/appuser/bot-detector/logs:ro`: Mounts the log directory from the host into the container in **read-only** mode.
*   `-v "$HOST_STATE_DIR":/home/appuser/bot-detector/state`: Mounts a directory from the host for persistence. This is **critical** for preserving the application's state (like active blocks) across container restarts. This volume **must** be read-write.
*   `--publish $BOTDET_API_PORT:$INTERNAL_PORT`: Exposes the application's web server port to the host machine.
*   `--config-dir "config"`: Points to the config directory *relative to the container's working directory* (`/home/appuser/bot-detector`). This directory must contain `config.yaml`.
*   `--state-dir "state"`: Enables persistence and tells the application to use the `state` directory (which is the volume mounted from the host) to store its data.
*   `--http-server "0.0.0.0:$INTERNAL_PORT"`: Tells the application to listen on all network interfaces inside the container, which is required for the published port to be accessible from the host.

### File Permissions and User Mapping

The container uses an [`entrypoint.sh`](../entrypoint.sh) script to handle file permissions when mounting host directories. By default, the application runs as user `appuser` (UID 1000, GID 1000).

**Matching Host User Permissions:**

If your host directories are owned by a different user, you can map the container's user to match your host user by setting the `PUID` and `PGID` environment variables:

```sh
# Get your host user's UID and GID
HOST_UID=$(id -u)
HOST_GID=$(id -g)

# Add to docker run command
docker run -d \
  --name "$CONTAINER_NAME" \
  --restart always \
  --env "PUID=$HOST_UID" \
  --env "PGID=$HOST_GID" \
  -v "$HOST_CONFIG_DIR":/home/appuser/bot-detector/config:ro \
  -v "$HOST_LOGS_DIR":/home/appuser/bot-detector/logs:ro \
  -v "$HOST_STATE_DIR":/home/appuser/bot-detector/state \
  --publish $BOTDET_API_PORT:$INTERNAL_PORT \
  "$IMAGE_NAME" \
  --config-dir "config" \
  --log-path "logs/$LOG_FILE_NAME" \
  --state-dir "state" \
  --http-server "0.0.0.0:$INTERNAL_PORT"
```

The entrypoint script will automatically adjust the container's internal user to match these IDs, ensuring proper file access to mounted volumes. This is particularly important for the `state` directory which requires write access.

**Note:** Running as root (PUID=0) is not supported for security reasons. The container will default to UID 1000 if root is specified.

### Running Commands on a Live Container

To run one-off commands like `--dump-backends` against a running container, use `docker exec`. This executes a new command inside the existing container without stopping it.

**Example: Checking Configuration**

To validate the configuration file inside a running container, you can use the `--check` flag. This is a great way to verify a new configuration before signaling the application to reload it.

```sh
docker exec "$CONTAINER_NAME" ./bot-detector --check --config-dir config
```
*   `"$CONTAINER_NAME"` is the name of the running container.
*   `./bot-detector --check ...` is the command to execute inside it.
*   Note that the path to the config directory (`config`) is relative to the container's working directory (`/home/appuser/bot-detector`).
