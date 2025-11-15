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
