# Roadmap to v2

> Status: **notes / proposal**, not a committed plan. bot-detector grew quickly
> under operational pressure and works well in production, but several seams are
> now showing. This is a short, evidence-based list of where a v2 should go.
> For the persistence-format/memory work, see
> [`v2-optimization-roadmap.md`](v2-optimization-roadmap.md) (complementary, not
> duplicated here).

## Why v2

The app is effective but organically grown. Concrete friction observed while
extending it:

- **No API contract.** 39 HTTP routes, no OpenAPI spec. Clients (botctl, the
  challenge frontend, ops scripts) reverse-engineer shapes from code.
- **Inconsistent API conventions.** Path verbs (`DELETE /ip/{ip}/clear`) vs
  query flags (`?unblock`), `?reason=` vs `?all`, plain-text `/stats/*` vs JSON
  `/api/v1/*` duplicates, uneven `/api/v1` prefixing.
- **Overloaded endpoints.** `DELETE /api/v1/bad-actors` handles reason-scoped
  *and* clear-all with mutual-exclusion logic in the handler.
- **Union response shapes.** `/ip/{ip}` returns ~4 different shapes
  (standalone / cluster / bad-actor / score) merged into one loose object —
  the direct cause of the botctl union struct and the cluster-reason display
  bug.
- **Public and internal APIs share a namespace.** Leader↔follower
  `/api/v1/cluster/internal/*` endpoints sit next to public ones, guarded only
  by a role tag and convention.
- **Substring reason matching** everywhere is fragile (cannot match
  null-history entries; easy to over-match).
- **Large, multi-responsibility files:** `handlers_ip.go` (1733 LOC),
  `config.go` (1729), `haproxy.go` (1560), `checker.go` (1123).
- **Config struct duplication:** ~25 `*ConfigYAML` ⇄ runtime struct pairs;
  every new field must be added in 2–3 places (see `AGENTS.md`).
- **41-method `server.Provider` interface** couples the server to the whole
  processor; every new capability touches the interface and all mock providers.

## Proposal

### 1. API v2 with an OpenAPI spec as the source of truth ⭐
- Author `openapi.yaml` first; generate server stubs and (partly) the botctl
  client + docs from it. The current `Botctl` endpoint marker is a stopgap for
  exactly this.
- Consistent resource modeling: `/api/v2/blocks`, `/api/v2/bad-actors`,
  `/api/v2/challenges`, `/api/v2/ips/{ip}` with standard verbs and
  sub-resources; retire path/query verb inconsistencies.
- **One stable schema per resource** (typed optional fields), not merged
  unions. Separate `IPStatus` cleanly from `BadActor`/`Score`/cluster
  aggregation.
- Consistent error envelope and status codes.
- Run v1 and v2 side by side; deprecate v1 once clients migrate.

### 2. Separate the internal cluster protocol from the public API
- Move `/cluster/internal/*` to its own namespace/listener (or port), with its
  own versioning. It is an RPC channel, not a public API.

### 3. Structured filters instead of substring reasons
- Match on explicit fields (chain name + vhost), not substrings. Makes
  reason-scoped removal/unblock precise and lets it reach all entries.

### 4. Reduce coupling
- Split `server.Provider` (41 methods) into role-focused interfaces
  (IP ops, bad actors, challenges, cluster, metrics). Smaller mocks, clearer
  boundaries.
- Break up the largest files along responsibility lines.

### 5. Config ergonomics
- Collapse the `*ConfigYAML` ⇄ runtime duplication (e.g. one tagged struct +
  a validation/normalization pass), removing the "edit in 3 places" chore.

### 6. Persistence
- Execute the memory/format work already scoped in
  [`v2-optimization-roadmap.md`](v2-optimization-roadmap.md) (drop
  `ActiveBlocks` redundancy, binary IP keys).

## Suggested sequencing

1. Write the OpenAPI spec for the **current** behavior (documents v1, forces
   clarity, immediately useful to clients).
2. Introduce `/api/v2` for the worst offenders (IP status, bad-actor removal),
   generated from the spec.
3. Split the internal cluster protocol out.
4. Refactor `Provider` + large files opportunistically as v2 handlers land.
5. Config and persistence changes as independent tracks.

## Non-goals

- No big-bang rewrite. v1 stays until v2 clients exist.
- Detection/matcher logic is sound; this roadmap is about structure, contracts,
  and ergonomics, not rules.
