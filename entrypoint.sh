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
echo "Setting user and group ID for appuser to $PUID:$PGID"

# Modify the existing 'appgroup' to match the provided PGID.
# This command needs to run as root.
groupmod -g "$PGID" appgroup

# Modify the existing 'appuser' to match the provided PUID and assign to 'appgroup'.
# This command needs to run as root.
usermod -u "$PUID" -g appgroup appuser

# Ensure the top-level application directory is owned by the correct user.
# This allows for creating/removing subdirectories like config.backup.
chown "$PUID:$PGID" /home/appuser/bot-detector

# Take ownership of writable application subdirectories (mount points and internal directories).
# The -R is important for the *contents* of these directories.
chown -R "$PUID:$PGID" /home/appuser/bot-detector/config
chown -R "$PUID:$PGID" /home/appuser/bot-detector/state
# The config.backup directory is created by the app within /home/appuser/bot-detector.
# This ensures its contents (if any exist) are also properly owned.
chown -R "$PUID:$PGID" /home/appuser/bot-detector/config.backup || true

# Execute the main application command, passing along all arguments.
# su-exec drops root privileges and runs the command as the specified user.
echo "Executing command as appuser: ./bot-detector $@"
exec su-exec appuser ./bot-detector "$@"
