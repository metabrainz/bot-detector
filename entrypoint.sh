#!/bin/sh
set -e

# Default to the 'appuser' if PUID/PGID are not set or if PUID is root (0)
PUID=${PUID:-1000}
PGID=${PGID:-1000}

# If the provided user is root, default to a non-root user for security.
# The user 'appuser' is created in the Dockerfile with UID/GID 1000.
if [ "$PUID" -eq 0 ]; then
    echo "WARNING: Running as root is not supported. Switching to user 'appuser'."
    PUID=1000
    PGID=1000
fi

# Set the user and group IDs for 'appuser' to match the host user.
# This allows the container to write to mounted volumes.
echo "Setting user and group ID to $PUID:$PGID"
deluser appuser >/dev/null 2>&1 || true
addgroup -g "$PGID" appgroup >/dev/null 2>&1
adduser -u "$PUID" -G appgroup -h /home/appuser -s /bin/sh -D appuser >/dev/null 2>&1

# Take ownership of the application directories.
# This is necessary for features like config backup to work correctly with mounted volumes.
echo "Taking ownership of /home/appuser/bot-detector"
chown -R "$PUID:$PGID" /home/appuser/bot-detector

# Execute the main application command, passing along all arguments.
# su-exec drops root privileges and runs the command as the specified user.
echo "Executing command as appuser: ./bot-detector $@"
exec su-exec appuser ./bot-detector "$@"
