# Bot-Detector Persistence and State Management

This document outlines the architecture used by `bot-detector` to reliably persist its state, ensuring that block information can be restored after a restart.

## Enabling Persistence

Persistence is disabled by default. To enable it, you **must** specify a state directory using the `--state-dir` command-line flag. This flag is the sole method for enabling persistence and setting the storage path.

```sh
./bot-detector --config config.yaml --state-dir /var/lib/bot-detector/state
```

You can optionally control other persistence behaviors, like the compaction interval, from your `config.yaml` file. If `persistence.enabled` is set to `true` in the YAML, the `--state-dir` flag becomes mandatory.

```yaml
# config.yaml
persistence:
  enabled: true
  compaction_interval: "1h" # Optional, defaults to 1 hour
```

If a `state_dir` is provided, the application will create the directory if it doesn't exist and manage its state within it.

## Guiding Principles

- **Local State is Truth:** The state stored locally by `bot-detector` is the source of truth. The backend's (e.g., HAProxy) state is considered a replica that will be automatically synchronized on startup.
- **Reliability:** The system prioritizes durably recording every action to prevent data loss.
- **Backend Agnostic:** The persistence model is self-contained and does not depend on the specific backend it manages.

## Core Components

The persistence model is a "Log-and-Snapshot" design, which relies on two key files within the configured `state_dir`:

### 1. The Journal: `events.log`

This is an append-only log file where every state-changing action is recorded *before* it is executed.

- **Format:** JSON Lines (JSONL). Each line is a complete JSON object representing a single event.
- **Content:** Records all `block` and `unblock` events, including timestamp, IP, duration, and the reason for the action.
- **Durability:** When a new event is logged, the file is explicitly flushed to disk (`fsync`). This guarantees that if the application crashes, a record of the intended action exists and will be correctly applied on the next startup.
- **Lifecycle:** This log grows during operation and is cleared by the Compaction process. It only contains events that have occurred since the last snapshot.

### 2. The Snapshot: `state.snapshot`

This file is a complete, point-in-time snapshot of all *active* blocks known to the system.

- **Format:** A single JSON object.
- **Content:** Contains the timestamp of the snapshot and a map of all currently blocked IPs to their calculated unblock time and reason.
- **Reliability:** The snapshot is written using an **atomic rename** pattern (`write to .tmp -> fsync -> rename`). This ensures that a valid, non-corrupt snapshot is always available, even if the application crashes mid-write.
- **Purpose:** Its primary role is to ensure fast application startups by avoiding the need to replay a long history of events.

## Key Processes

### Startup and State Restoration

On startup, `bot-detector` follows a specific process to bring the backend's state into sync with its local source of truth.

1.  **Load Snapshot:** The application loads the `state.snapshot` into memory. If the file doesn't exist, it starts with an empty state.
2.  **Replay Journal:** It reads `events.log` and applies any events that are newer than the snapshot's timestamp, ensuring the in-memory state is fully up-to-date.
3.  **State Push:** `bot-detector` iterates through its complete in-memory list of active blocks and issues a `block` command to the configured backend for each one. This process is idempotent and ensures the backend converges to the correct state without causing a service interruption.

### Compaction

To prevent the `events.log` from growing infinitely, a periodic compaction process runs. The interval is configurable via `compaction_interval` in the YAML config (default is 1 hour).

1.  **Snapshot:** The application creates a new, clean snapshot of the current in-memory state (after purging any expired entries) using the atomic rename pattern.
2.  **Truncate:** Once the new snapshot is safely on disk, the `events.log` is truncated (cleared), as all its information is now consolidated in the new snapshot.

## Failure Scenarios

| Failure Type | System Behavior | Outcome | Key Feature |
| :--- | :--- | :--- | :--- |
| **App Crash (Normal)** | Replays journal on restart to recover state since last snapshot. | **Highly Resilient.** State is self-healed. | Journaling (`events.log`) |
| **App Crash (Compaction)** | Safely ignores temporary files and uses the last good snapshot and journal. | **Highly Resilient.** No state is lost. | Atomic Rename |
| **Disk Full** | Write operations fail; the app logs errors and may halt. | **Safe.** Prevents state inconsistency. | Error Handling on I/O |
| **Network Failure** | A single block command is retried based on `blocker_max_retries`. | **Resilient.** The state will be fully synchronized on the next restart. | Per-Command Retries |

### Crash During Normal Operation

If the application crashes after writing an event to the journal but before executing it on the backend, the startup process will automatically correct the situation. The journaled event is replayed, added to the in-memory state, and pushed to the backend, ensuring no actions are lost.

### Crash During Compaction

The compaction process is transactional. If a crash occurs, the state remains consistent because the old snapshot and journal are not deleted until the new snapshot is successfully written.

## Disaster Recovery from a Snapshot Backup

If the entire server is lost, the `state.snapshot` file is the key asset for recovery.

### Restoration Procedure

1.  **Prepare New Server:** Set up a new machine with the `bot-detector` binary and its backend.
2.  **Restore Snapshot:** Place the backed-up `state.snapshot` file into the state directory. **Ensure no `events.log` file is present.**
3.  **Start Application:** Launch `bot-detector`. It will load the snapshot, see there is no journal to replay, and proceed to push the state to the backend.
4.  **Resume:** The system will create a new `events.log` and resume normal operation.

This means your Recovery Point Objective (RPO) is determined by how frequently you back up the `state.snapshot` file.