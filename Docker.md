# Docker

## Building the image

```bash
#!/bin/bash


docker build -t bot-detector:latest .
```

## Dry run / test

```bash

#!/bin/bash

HOST_TEST_LOG="./test_access.log"
HOST_CONFIG_PATH="./chains.yaml"

# Define container paths (these should match the flags)
CONTAINER_APP_DIR="/home/appuser/bot-detector"
CONTAINER_TEST_LOG="${CONTAINER_APP_DIR}/test_access.log"
CONTAINER_CONFIG_PATH="${CONTAINER_APP_DIR}/chains.yaml"

# NOTE: Dry run mode does not require HAProxy socket or map file mounts.

docker run --rm \
    --name bot-detector-dry-run \
    -v ${HOST_TEST_LOG}:${CONTAINER_TEST_LOG}:ro \
    -v ${HOST_CONFIG_PATH}:${CONTAINER_CONFIG_PATH} \
    bot-detector:latest \
    --dry-run \
    --test-log "${CONTAINER_TEST_LOG}" \
    --yaml-path "${CONTAINER_CONFIG_PATH}"
```

## Deploying a container

```bash

# Define host paths based on common HAProxy and log setups
HOST_LOG_PATH="/var/log/http/access.log"
HOST_SOCKET_PATH="/run/haproxy/admin.sock"  # if using socket to comuunicate with HAProxy
HOST_CONFIG_PATH="./chains.yaml" # Assuming chains.yaml is in the directory where you run this command

# Define container paths (these should match the defaults or flags used by bot-detector)
CONTAINER_APP_DIR="/home/appuser/bot-detector"
CONTAINER_LOG_PATH="${CONTAINER_APP_DIR}/access.log"
CONTAINER_SOCKET_PATH="${CONTAINER_APP_DIR}/haproxy.sock"
CONTAINER_CONFIG_PATH="${CONTAINER_APP_DIR}/chains.yaml"

docker run -d \
    --name bot-detector-instance \
    --restart unless-stopped \
    -v ${HOST_LOG_PATH}:${CONTAINER_LOG_PATH}:ro \
    -v ${HOST_SOCKET_PATH}:${CONTAINER_SOCKET_PATH} \
    -v ${HOST_CONFIG_PATH}:${CONTAINER_CONFIG_PATH} \
    bot-detector:latest \
    --log-path "${CONTAINER_LOG_PATH}" \
    --yaml-path "${CONTAINER_CONFIG_PATH}"
```
