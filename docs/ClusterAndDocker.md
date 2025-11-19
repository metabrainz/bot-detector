# Docker and Containerized Deployment Guide

This guide covers deploying bot-detector in containerized environments (Docker, Kubernetes) with the enhanced cluster features for container-friendly configuration.

## Table of Contents

1. [Overview](#overview)
2. [Node Identification with --cluster-node-name](#node-identification-with---cluster-node-name)
3. [Dynamic Configuration with BOT_DETECTOR_NODES](#dynamic-configuration-with-bot_detector_nodes)
4. [Name-Based FOLLOW Files](#name-based-follow-files)
5. [Docker Compose Example](#docker-compose-example)
6. [Kubernetes Deployment](#kubernetes-deployment)
7. [Migration Guide](#migration-guide)
8. [Troubleshooting](#troubleshooting)

## Overview

Traditional cluster configuration relies on static IP addresses and ports, which conflicts with container orchestration where:
- Internal container ports differ from external published ports
- Container IP addresses are dynamic and managed by the orchestrator
- Service discovery uses DNS names rather than IP addresses

This guide introduces three features that enable seamless containerized deployments:
1. **--cluster-node-name**: Explicit node identification independent of listen address
2. **BOT_DETECTOR_NODES**: Environment variable for dynamic cluster topology
3. **Name-based FOLLOW**: Using node names instead of addresses in FOLLOW files

For general cluster architecture and concepts, see [ClusterConfiguration.md](ClusterConfiguration.md).

## Node Identification with --cluster-node-name

### The Problem

In traditional deployments, bot-detector identifies which node it represents by matching its listen address against the `cluster.nodes` list in config.yaml. For example:

```yaml
# config.yaml
cluster:
  nodes:
    - name: node-1
      address: "192.168.1.10:8080"
```

```bash
# Node identifies as "node-1" because it listens on :8080 which matches the port
bot-detector --listen=:8080
```

This breaks in containers because:
- The container listens internally on `:8080`
- But the external address is the service name: `node-1:8080`
- Port matching fails when internal and external ports differ

### The Solution

The `--cluster-node-name` flag explicitly specifies node identity:

```bash
bot-detector --cluster-node-name=node-1 --config=/config
```

This works regardless of listen address or port mapping.

### Usage

**Required in:**
- Docker Compose deployments
- Kubernetes StatefulSets/Deployments
- Any environment with port mapping or service meshes

**Optional in:**
- Traditional VMs where listen address matches cluster address
- Single-node deployments

**Example:**
```bash
# Docker container
docker run \
  -e BOT_DETECTOR_NODES="leader:leader:8080;follower:follower:8080" \
  bot-detector:latest \
  --cluster-node-name=leader \
  --config=/config
```

### Fallback Behavior

If `--cluster-node-name` is not provided, bot-detector attempts address matching for backward compatibility. This works in traditional deployments but will fail in containers.

## Dynamic Configuration with BOT_DETECTOR_NODES

### The Problem

Static cluster configuration in config.yaml doesn't work well with containers:
- Service names vary by environment (dev, staging, prod)
- Cannot use environment variables in YAML
- Requires building different images or config files per environment

### The Solution

The `BOT_DETECTOR_NODES` environment variable enables runtime cluster configuration:

```bash
export BOT_DETECTOR_NODES="leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080"
```

### Format

```
BOT_DETECTOR_NODES="nodename1:address1;nodename2:address2;..."
```

- **Semicolon** (`;`) separates node entries
- **Colon** (`:`) separates name from address within each entry
- **First colon** is the name/address separator (supports addresses with colons like URLs and IPv6)

### Examples

**Simple two-node cluster:**
```bash
BOT_DETECTOR_NODES="leader:leader:8080;follower:follower:8080"
```

**Multiple followers:**
```bash
BOT_DETECTOR_NODES="leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080;follower-3:follower-3:8080"
```

**IPv6 addresses:**
```bash
BOT_DETECTOR_NODES="leader:[::1]:8080;follower:[::1]:9090"
```

**Full URLs:**
```bash
BOT_DETECTOR_NODES="leader:http://leader.svc.cluster.local:8080;follower:http://follower.svc.cluster.local:8080"
```

**External DNS:**
```bash
BOT_DETECTOR_NODES="leader:leader.example.com:8080;follower:follower.example.com:8080"
```

### Behavior

**Complete Replacement:**
When `BOT_DETECTOR_NODES` is set, it completely replaces `cluster.nodes` from config.yaml. This is intentional - the environment variable takes full precedence.

**Other Settings Preserved:**
Only the nodes list is replaced. Other cluster settings still come from config.yaml:
- `config_poll_interval`
- `metrics_report_interval`
- `protocol`

Or use defaults if not specified in config.yaml.

**Critical Requirement:**
All nodes in the cluster MUST have identical `BOT_DETECTOR_NODES` values. Each node needs the complete topology:
- Leaders use it to know which followers to poll for metrics
- Followers use it to resolve leader names in FOLLOW files
- Both use it for node identification

### Docker Compose Example

```yaml
services:
  leader:
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"
  
  follower:
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"  # Same!
```

### Kubernetes ConfigMap Example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: bot-detector-cluster
data:
  CLUSTER_NODES: "leader:bot-detector-leader:8080;follower-0:bot-detector-follower-0:8080;follower-1:bot-detector-follower-1:8080"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bot-detector-leader
spec:
  template:
    spec:
      containers:
      - name: bot-detector
        env:
        - name: BOT_DETECTOR_NODES
          valueFrom:
            configMapKeyRef:
              name: bot-detector-cluster
              key: CLUSTER_NODES
```

## Name-Based FOLLOW Files

### The Problem

Traditional FOLLOW files contain the leader's full address:

```bash
# FOLLOW file
http://192.168.1.10:8080
```

This breaks in containers because:
- IP addresses are dynamic
- Service names vary by environment
- Different namespaces use different DNS names

### The Solution

Use the leader's **node name** instead of its address:

```bash
# FOLLOW file
leader
```

Bot-detector resolves the name to an address using the cluster configuration (from config.yaml or `BOT_DETECTOR_NODES`).

### Benefits

1. **Environment Independence**: Same FOLLOW file works in dev, staging, and prod
2. **Simpler Configuration**: Just "leader" instead of full URL
3. **Centralized Management**: Change addresses by updating cluster config, not FOLLOW files
4. **Container-Friendly**: Use service names directly

### Resolution Process

When reading the FOLLOW file, bot-detector determines if the content is an address or a name:

**Treated as Address (backward compatible):**
- Contains `://` → URL (e.g., `http://leader:8080`)
- Contains `:` with numeric port → host:port (e.g., `leader:8080`, `192.168.1.10:8080`)
- Starts with `[` → IPv6 (e.g., `[::1]:8080`)

**Treated as Name (new feature):**
- None of the above → node name (e.g., `leader`, `primary-node`)
- Resolved by looking up the name in `cluster.nodes` or `BOT_DETECTOR_NODES`

### Examples

**Name-based (recommended for containers):**
```bash
echo "leader" > /config/FOLLOW
```

**Traditional address-based (backward compatible):**
```bash
echo "http://192.168.1.10:8080" > /config/FOLLOW
```

**Host:port format (backward compatible):**
```bash
echo "192.168.1.10:8080" > /config/FOLLOW
```

### Requirements

For name-based FOLLOW to work:
- Cluster configuration must be available (config.yaml or `BOT_DETECTOR_NODES`)
- The referenced node name must exist in the cluster nodes list

During bootstrap (when follower has no config.yaml yet):
- `BOT_DETECTOR_NODES` must be set
- The leader node name must be in `BOT_DETECTOR_NODES`

## Docker Compose Example

Complete working example with all features:

### Directory Structure

```
bot-detector-cluster/
├── docker-compose.yml
├── leader/
│   └── config/
│       └── config.yaml
└── follower/
    └── config/
        └── FOLLOW
```

### docker-compose.yml

```yaml
version: '3.8'

services:
  leader:
    image: bot-detector:latest
    container_name: bot-detector-leader
    hostname: leader
    ports:
      - "8080:8080"
    volumes:
      - ./leader/config:/config
    environment:
      # Cluster topology - identical on all nodes
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"
    command:
      - "--config=/config"
      - "--cluster-node-name=leader"
    networks:
      - cluster-net
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 3

  follower:
    image: bot-detector:latest
    container_name: bot-detector-follower
    hostname: follower
    ports:
      - "9090:8080"
    volumes:
      - ./follower/config:/config
    environment:
      # Same cluster topology as leader (critical!)
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"
    command:
      - "--config=/config"
      - "--cluster-node-name=follower"
    networks:
      - cluster-net
    depends_on:
      leader:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 3

networks:
  cluster-net:
    driver: bridge
```

### leader/config/config.yaml

```yaml
version: "1.0"

http:
  listen_address: ":8080"

# Cluster settings (nodes will come from BOT_DETECTOR_NODES)
cluster:
  config_poll_interval: "10s"
  metrics_report_interval: "30s"
  protocol: "http"
  # nodes: []  # Omit or leave empty - will be populated by environment variable

# Your behavioral chains, blockers, etc.
chains:
  - name: "http2_scanner"
    # ... chain config ...

blocker_addresses:
  - "haproxy:9999"
```

### follower/config/FOLLOW

```
leader
```

Just the node name! Bot-detector will:
1. Read `BOT_DETECTOR_NODES` environment variable
2. Resolve "leader" to "leader:8080"
3. Bootstrap config.yaml from `http://leader:8080/config/archive`
4. Start as follower

### Running the Cluster

```bash
# Build the image
docker build -t bot-detector:latest .

# Create FOLLOW file for follower
mkdir -p follower/config
echo "leader" > follower/config/FOLLOW

# Start the cluster
docker-compose up -d

# Check status
curl http://localhost:8080/cluster/status  # Leader
curl http://localhost:9090/cluster/status  # Follower

# View logs
docker-compose logs -f
```

### Expected Status Output

**Leader:**
```json
{
  "role": "leader",
  "name": "leader",
  "address": "leader:8080"
}
```

**Follower:**
```json
{
  "role": "follower",
  "name": "follower",
  "address": "follower:8080",
  "leader": "leader:8080"
}
```

### Scaling to Multiple Followers

```yaml
version: '3.8'

services:
  leader:
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080;follower-3:follower-3:8080"
    # ... rest same as above ...

  follower-1:
    hostname: follower-1
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080;follower-3:follower-3:8080"
    command:
      - "--cluster-node-name=follower-1"
    # ... rest similar to follower ...

  follower-2:
    hostname: follower-2
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080;follower-3:follower-3:8080"
    command:
      - "--cluster-node-name=follower-2"

  follower-3:
    hostname: follower-3
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower-1:follower-1:8080;follower-2:follower-2:8080;follower-3:follower-3:8080"
    command:
      - "--cluster-node-name=follower-3"
```

## Kubernetes Deployment

Example StatefulSet deployment:

### Namespace and ConfigMap

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: bot-detector
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-topology
  namespace: bot-detector
data:
  # All nodes in the cluster - update this when scaling
  CLUSTER_NODES: "leader:bot-detector-leader:8080;follower-0:bot-detector-follower-0.bot-detector-follower:8080;follower-1:bot-detector-follower-1.bot-detector-follower:8080"
```

### Leader Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bot-detector-leader
  namespace: bot-detector
spec:
  replicas: 1
  selector:
    matchLabels:
      app: bot-detector
      role: leader
  template:
    metadata:
      labels:
        app: bot-detector
        role: leader
    spec:
      containers:
      - name: bot-detector
        image: bot-detector:latest
        args:
          - "--config=/config"
          - "--cluster-node-name=leader"
        ports:
        - containerPort: 8080
          name: http
        env:
        - name: BOT_DETECTOR_NODES
          valueFrom:
            configMapKeyRef:
              name: cluster-topology
              key: CLUSTER_NODES
        volumeMounts:
        - name: config
          mountPath: /config
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
      volumes:
      - name: config
        configMap:
          name: bot-detector-config  # Your main config
---
apiVersion: v1
kind: Service
metadata:
  name: bot-detector-leader
  namespace: bot-detector
spec:
  selector:
    app: bot-detector
    role: leader
  ports:
  - port: 8080
    targetPort: 8080
  type: ClusterIP
```

### Follower StatefulSet

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: bot-detector-follower
  namespace: bot-detector
spec:
  serviceName: bot-detector-follower
  replicas: 2
  selector:
    matchLabels:
      app: bot-detector
      role: follower
  template:
    metadata:
      labels:
        app: bot-detector
        role: follower
    spec:
      containers:
      - name: bot-detector
        image: bot-detector:latest
        args:
          - "--config=/config"
          - "--cluster-node-name=$(POD_NAME)"
        ports:
        - containerPort: 8080
          name: http
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: BOT_DETECTOR_NODES
          valueFrom:
            configMapKeyRef:
              name: cluster-topology
              key: CLUSTER_NODES
        volumeMounts:
        - name: config
          mountPath: /config
        - name: follow-file
          mountPath: /config/FOLLOW
          subPath: FOLLOW
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
      volumes:
      - name: config
        configMap:
          name: bot-detector-config
      - name: follow-file
        configMap:
          name: follower-follow
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: follower-follow
  namespace: bot-detector
data:
  FOLLOW: "leader"
---
apiVersion: v1
kind: Service
metadata:
  name: bot-detector-follower
  namespace: bot-detector
spec:
  clusterIP: None  # Headless service for StatefulSet
  selector:
    app: bot-detector
    role: follower
  ports:
  - port: 8080
    targetPort: 8080
```

### Scaling

```bash
# Scale followers
kubectl scale statefulset bot-detector-follower --replicas=5 -n bot-detector

# Update cluster topology in ConfigMap (don't forget this!)
kubectl edit configmap cluster-topology -n bot-detector
# Add: follower-2, follower-3, follower-4 to CLUSTER_NODES

# Restart pods to pick up new topology
kubectl rollout restart deployment/bot-detector-leader -n bot-detector
kubectl rollout restart statefulset/bot-detector-follower -n bot-detector
```

## Migration Guide

### From Traditional VMs to Docker

**Before:**
```yaml
# config.yaml on all VMs
cluster:
  nodes:
    - name: vm-1
      address: "192.168.1.10:8080"
    - name: vm-2
      address: "192.168.1.11:8080"
```

```bash
# Start on vm-1
bot-detector --config=/etc/bot-detector

# FOLLOW file on vm-2
echo "192.168.1.10:8080" > /etc/bot-detector/FOLLOW
```

**After:**
```yaml
# docker-compose.yml
services:
  leader:
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"
    command: ["--cluster-node-name=leader"]
  
  follower:
    environment:
      BOT_DETECTOR_NODES: "leader:leader:8080;follower:follower:8080"
    command: ["--cluster-node-name=follower"]
```

```bash
# FOLLOW file
echo "leader" > follower/config/FOLLOW
```

### From Implicit to Explicit Node Names

**Before (relies on port matching):**
```bash
# Leader listens on :8080, matches cluster.nodes entry with port 8080
bot-detector --config=/config
```

**After (explicit and clear):**
```bash
# Clear identity regardless of ports
bot-detector --cluster-node-name=leader --config=/config
```

## Troubleshooting

### "node 'X' provided via --cluster-node-name not found in cluster configuration"

**Cause:** The specified node name doesn't exist in `cluster.nodes` or `BOT_DETECTOR_NODES`.

**Solution:**
- Verify `BOT_DETECTOR_NODES` contains the node name
- Check spelling and case sensitivity
- Ensure all nodes have identical cluster topology

### "FOLLOW file contains node name 'X', but no cluster configuration available"

**Cause:** Using name-based FOLLOW but cluster config isn't loaded.

**Solution:**
- Set `BOT_DETECTOR_NODES` environment variable
- Ensure config.yaml has `cluster.nodes` section
- For bootstrap, `BOT_DETECTOR_NODES` is required

### "FOLLOW file contains node name 'X', but no such node found in cluster configuration"

**Cause:** Leader name in FOLLOW file isn't in the nodes list.

**Solution:**
- Add leader to `BOT_DETECTOR_NODES`
- Or use direct address format in FOLLOW file: `echo "http://leader:8080" > FOLLOW`

### Follower Can't Reach Leader in Docker

**Symptoms:** Bootstrap fails, config sync fails, connection refused errors.

**Diagnosis:**
```bash
# From follower container, test connectivity
docker exec bot-detector-follower ping leader
docker exec bot-detector-follower curl http://leader:8080/health
```

**Solutions:**
- Ensure both containers are on the same Docker network
- Use service/hostname in `BOT_DETECTOR_NODES`, not `localhost`
- Check Docker network with `docker network inspect`
- Verify leader is healthy with health check

### BOT_DETECTOR_NODES Mismatch Between Nodes

**Symptoms:** Metrics not aggregating, nodes not appearing in cluster status.

**Cause:** Different nodes have different `BOT_DETECTOR_NODES` values.

**Solution:**
- Use shared environment file in Docker Compose
- Use ConfigMap in Kubernetes
- Verify with: `docker exec <container> env | grep BOT_DETECTOR_NODES`

### Config Not Syncing to Followers

**Diagnosis:**
```bash
# Check follower can reach leader
curl http://leader:8080/config/archive

# Check follower status
curl http://localhost:9090/cluster/status

# Check logs
docker logs bot-detector-follower
```

**Common Causes:**
- Leader address incorrect in FOLLOW file or cluster config
- Network connectivity issues
- Leader not serving `/config/archive` endpoint

## Best Practices

1. **Always use --cluster-node-name in containers**
   - Required for correct node identification
   - Makes configuration explicit and debuggable

2. **Keep BOT_DETECTOR_NODES identical on all nodes**
   - Use ConfigMaps (Kubernetes) or shared environment files (Docker Compose)
   - Validate with scripts before deployment

3. **Use name-based FOLLOW files**
   - More maintainable than hardcoded addresses
   - Environment-independent

4. **Health checks are critical**
   - Required for depends_on in Docker Compose
   - Used by Kubernetes for readiness/liveness
   - Prevents followers from starting before leader is ready

5. **Monitor cluster topology changes**
   - When scaling, update `BOT_DETECTOR_NODES` everywhere
   - Restart/rollout to pick up changes
   - Verify with `/cluster/status` endpoint

6. **Use service discovery**
   - Docker Compose: Use service names as hostnames
   - Kubernetes: Use Service DNS names
   - Don't hardcode IP addresses
