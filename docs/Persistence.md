# Bot-Detector Persistence and State Management

This document outlines the architecture used by `bot-detector` to reliably persist its state, ensuring that no block information is lost and that the system can be restored perfectly after a restart.

## Guiding Principles

- **Single Source of Truth:** The state stored locally by `bot-detector` is the absolute source of truth. The backend's (e.g., HAProxy) state is considered a disposable replica that will be automatically corrected to match the source of truth.
- **Reliability over Performance:** The system prioritizes guaranteeing that every action is durably recorded, accepting minor performance trade-offs to prevent data loss.
- **Backend Agnostic:** The persistence model is self-contained and does not depend on any specific features of the backend it manages.

## Core Components

The persistence model is a "Log-and-Snapshot" design, which relies on two key files:

### 1. The Journal: `events.log`

This is an append-only log file where every action is recorded *before* it is executed.

- **Format:** JSON Lines (JSONL). Each line is a complete JSON object representing a single event.
- **Content:** Records all `block` and `unblock` events, including timestamp, IP, duration, and a reason for the action.
- **Durability:** When a new event is logged, the file is explicitly flushed to disk (`fsync`). This guarantees that even if the application crashes immediately after, a record of the intended action exists and will be correctly applied on the next startup.
- **Lifecycle:** This log grows during normal operation and is periodically cleared by the Compaction process. It only ever contains events that have occurred since the last snapshot.

### 2. The Snapshot: `state.snapshot`

This file is a complete, point-in-time snapshot of all *active* blocks known to the system.

- **Format:** A single JSON object.
- **Content:** Contains the timestamp of when the snapshot was taken and a dictionary of all currently blocked IPs and their calculated `unblock_time`.
- **Reliability:** The snapshot is written using an **atomic rename** pattern (`write to .tmp -> fsync -> rename`). This ensures that a valid, non-corrupt snapshot is always available, even if the application crashes mid-write.
- **Purpose:** Its primary role is to ensure fast application startups by avoiding the need to replay the entire history of events.

## Key Processes

### Startup and State Restoration

`bot-detector` follows a specific process on startup to ensure the backend's state is brought into perfect sync with its source of truth.

1.  **Load State:** The application loads the `state.snapshot` into memory, then replays any newer events from `events.log` to build a complete and current in-memory state.
2.  **Idempotent State Push:** `bot-detector` iterates through its complete in-memory state and sends a `set table ... gpc0 1` command to the backend for every IP that should be blocked.
    - This process does **not** clear the backend tables first.
    - The `set table` command is idempotent: it creates an entry if it's missing or updates it if it already exists. This ensures the backend converges to the correct state without a service interruption or a vulnerable "open window."

### Compaction

To prevent the `events.log` from growing infinitely, a periodic compaction process runs at a tunable interval.

1.  **Pause:** The application briefly pauses processing new input.
2.  **Snapshot:** It creates a new, clean snapshot of the current in-memory state (after purging any expired entries) using the atomic rename pattern.
3.  **Truncate:** Once the new snapshot is safely on disk, the `events.log` is truncated (cleared), as all its information is now consolidated in the snapshot.
4.  **Resume:** The application resumes normal operation.

## Failure Scenario Analysis

This architecture is designed to be resilient to common failures.

| Failure Type | System Behavior | Outcome | Key Feature |
| :--- | :--- | :--- | :--- |
| **App Crash (Normal)** | Replays journal on restart to find un-executed actions. | **Highly Resilient.** State is self-healed. | Journaling (`events.log`) + `fsync` |
| **App Crash (Compaction)** | Safely ignores temporary files and replays the old log. | **Highly Resilient.** No state is lost. | Atomic Rename |
| **Disk Full** | Write operations fail; the app halts processing new events. | **Safe.** Prevents state inconsistency. | Error Handling on I/O |
| **File Corruption** | Can recover from the journal if the snapshot is corrupt. | **Resilient.** Fails safely if the journal is also lost. | Graceful Degradation |
| **Network Failure** | Retries backend commands in the background until successful. | **Highly Resilient.** Achieves eventual consistency. | Retry Queues |

### Crash During Normal Operation

If the application crashes after writing an event to the journal but before executing it on the backend, the startup process will automatically correct the situation. The journaled event is replayed, added to the in-memory state, and pushed to the backend, ensuring no actions are ever lost.

### Crash During Compaction

The compaction process is transactional. If a crash occurs:
- **Before the atomic `rename`:** The old snapshot and journal are used on the next startup. The temporary snapshot file is ignored.
- **After the `rename` but before the journal is cleared:** The new snapshot is used. The journal replay logic correctly ignores events that are older than the new snapshot, preventing double-application of events.

In all cases, the state remains consistent.

### Disk and Network Failures

The system is designed to fail safely.
- **On disk full:** It stops processing rather than continue with an un-journaled action that could lead to an inconsistent state.
- **On network failure:** It logs the error and persistently retries sending the state to the backend in the background until the connection is restored, ensuring eventual consistency.