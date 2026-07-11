# End-to-End Schema-Change Safety: Ready Snapshot, Version Gate, DDL Readiness Handshake, and Dropped-Field Tombstones

- Issue: #50989
- PR: #50990
- Date: 2026-07-06 (revised 2026-07-08: state-derived publish gate, schema-version
  read gate, write-path audit, DDL readiness handshake; revised 2026-07-09: function
  runner consistency rules, dropped-field tombstones merged in as Part II)

Part I covers **transition-time ordering**: a schema change must not be servable
before every shard delegator is actually ready for it. Part II covers **steady-state
name-space safety**: once a field is dropped, its name must not silently resurrect
through the dynamic field.

## Problem

A schema change (AddCollectionField / add function) becomes visible to clients the moment
rootcoord's broadcast completes and proxy caches expire — long before every QueryNode shard
delegator has actually made the new schema *servable*. Three concrete failure classes:

1. **Function runtime state raced with segment load ("load-wins")**: the delegator's
   function-output metadata was rebuilt only by the `UpdateSchema` WAL event, behind a
   freshness guard. A segment load (`PutOrRef`) advancing the collection schema first turned
   that event into a no-op, so a search against a MinHash/BM25 output field forwarded the raw
   VARCHAR placeholder to segcore (`Plan.cpp:163` data-type assert).
2. **BM25 stats visibility race**: a schema version could be exposed to reads while a reopen
   was still downloading/activating its sealed BM25 stats, letting a BM25 search pass the gate
   and silently return empty results (`avgdl <= 0`).
3. **No readiness contract**: `UpdateSchema` failures in the consume pipeline were
   warn-and-skipped (permanently stranding the delegator on the old schema, with later
   new-schema inserts silently dropping the new field), and DDL completion had no relationship
   to shard readiness, so "AddField returned" did not imply "new field usable".

## Design

### Two levels of schema state per shard delegator

| state | advanced by | serves | readiness requirement |
|---|---|---|---|
| **applied** (live collection schema) | WAL `UpdateSchema` (in order), or segment-load `PutOrRef` (load-wins) | write path (`ProcessInsert` growing creation), index meta | WAL order only |
| **published** (`readySnapshot`, RCU atomic pointer) | `tryPublishReadySchema()` only | read path (`search`/`Query`/`GetHighlight`/`RunAnalyzer`) | full checklist below |

`published ≤ applied` always. Readers load the snapshot lock-free; a snapshot is never mutated
after publish; publish is monotonic by `(schema version, barrier ts)`.

### State-derived publish checklist (replaces control-flow coordination)

`tryPublishReadySchema()` re-derives readiness from state on every call:

1. **Function runners ready** — `function.EnsureRunnersReady` synchronously initializes every
   runner of the version (no lazy read-path init; a failure is an error driven by the caller's
   retry loop).
2. **No pending BM25 stats loads** — `LoadSegments` marks stats-bearing segments in
   `pendingBM25Loads` *before* any step that can advance the applied schema, and always clears
   them on return (success or failure) followed by a publish re-attempt.
3. **idfOracle activated** — `idfOracle.Ready()`: every *loaded* sealed stats entry belonging
   to the current serving target is activated (merged into the aggregated stats).

Publishing additionally requires the schema to be known by the WORKERS, not just the
delegator: a published version admits reads compiled against it, and those reads fan out
to workers whose segcore must already hold the schema. The main `UpdateSchema` path always
fans the schema out to all workers; the load-wins path and the `UpdateSchema` no-op branch
(reached when a load advanced the schema first, which would otherwise skip the fan-out
entirely) perform the same worker fan-out before their publish attempt.

Conditions 2–3 failing is not an error: the attempt is a silent no-op and the read path keeps
serving the previous ready version; the next state change re-attempts. Call sites:
delegator creation, `UpdateSchema` (normal and no-op branches), `syncCollectionIndexMeta`
(load-wins), the deferred publish in `LoadSegments`, and the idfOracle activation callback
(`SetOnStatsActivated`, fired from `SyncDistribution`). Because every publisher goes through
the same gate, no ordering coordination between UpdateSchema / loads / reopen is needed —
the earlier `deferPublish`/`resumePublish` control-flow mechanism is removed.

**Progressive visibility, not exhaustive readiness.** `Ready()` intentionally does NOT require
every target segment to have stats: a segment absent from the oracle has nothing loadable yet
(its BM25 backfill — an *independent*, external job committed via
`DataCoord.CommitBackfillResult` — has not run). Requiring it would deadlock the gate against
backfill, which can only run after the DDL commits. Old rows become searchable through the new
field progressively as backfill + reopen proceed; IDF/avgdl are always consistent with the
currently indexed corpus. Only *in-flight* loads (pending set) and *loaded-but-unactivated*
entries gate publishing — both transient by construction, so the gate cannot deadlock.

The pending gate protects PUBLISHING; a version already published is never
retracted. For backfill-driven reopens that arrive after the publish, ordering
inside `LoadSegments` closes the window instead: the reopened segments' BM25
stats are downloaded and registered BEFORE the worker swaps the new column in
(a download failure aborts the reopen before the swap, and querycoord retries),
and activated right after the swap — so there is no interval in which the new
column is searchable while its stats are absent from the oracle (empty results
while avgdl is 0, silently skewed IDF otherwise).

### Read path: schema-version gate

The proxy attaches the schema version it compiled the request against
(`internalpb.SearchRequest/RetrieveRequest.collection_schema_version`, set in
search/query `PreExecute` and in the delete-by-expression read request built by
`deleteRunner`). The delegator serves only from the published snapshot and rejects
requests whose version is ahead of it with retriable `ErrCollectionSchemaVersionNotReady`
(code 110); the proxy LB policy retries until the shard catches up. Version 0 (legacy proxy /
rolling upgrade) skips the gate. Requests compiled against an older version are always served
(schema changes are additive for reads). This replaces the previous per-request plan-unmarshal
field-ID dependency check — the gate is now an O(1) integer compare with no plan decode on the
hot path.

### Write path (audited, already ordered — no new mechanism)

Inserts carry `SchemaVersion` in the message header; the streamingnode rejects
version-mismatched inserts before WAL append and applies AlterCollection synchronously
(fence + flush old growings, update schema and function runners) at append time. WAL total
order plus the pipeline rule below guarantees the delegator's applied schema is ≥ any insert's
version when `ProcessInsert` runs.

### Schema-change messages are never skipped

`pipeline/delete_node.go` retries a failed `delegator.UpdateSchema` in place with backoff
(`queryNode.schemaUpdateRetryTimes`, default 10), blocking that vchannel's consumption (tsafe
stops advancing; strongly-consistent reads stall; eventually-consistent reads keep serving the
old snapshot), and panics when exhausted so the querynode recovers by WAL replay. Skipping the
message would let subsequent new-schema inserts be processed against the old schema.

### DDL readiness handshake

- Delegator exposes `ReadySchemaVersion()` (published snapshot version, -1 before first
  publish) → reported in `querypb.LeaderView.ready_schema_version` via `GetDataDistribution`.
  Every ready publish bumps the querynode's distribution-modify timestamp: a pure
  schema publish changes no segment distribution, and without the bump the
  unchanged-distribution fast path would keep returning leader-view-less responses,
  hiding the new version from querycoord and stalling the handshake indefinitely.
- QueryCoord `CheckSchemaReady(collectionID, version)`: true iff the collection is not loaded,
  or every query-visible replica's shard leader view reports `ready_schema_version ≥ version`
  (not-ready when the collection is loaded but no replica is query-visible). This is an
  IN-PROCESS contract on the merged coordinator, deliberately not a service RPC: the readiness
  data flows to querycoord through the regular `GetDataDistribution` heartbeat, and the only
  caller — rootcoord's DDL callback — always lives in the same process, so there is no wire
  surface and no rolling-upgrade version-negotiation concern for this call.
- RootCoord's `alterCollectionV2AckCallback` (shared by all schema-bumping alters; detected by
  `Updates.Schema != nil`) polls `CheckSchemaReady` **before** `ExpireCaches`. Not-ready after
  a bounded poll returns an error, which the broadcast framework retries with backoff
  **indefinitely** (per-collection resource lock held, so same-collection DDLs serialize) —
  built-in background compensation. The client's AddField call therefore blocks until all
  shards are ready or its own deadline; either way caches expire only after readiness.

The delegator version gate remains the correctness backstop for schemas observed early
(fresh proxies, cache misses, rebalancing leaders).

### Function runner consistency across schema versions

Read-path runners resolve at `LatestFunctionRunnerVersion`: runners are keyed by
function signature and shared across versions, so a latest-version runner is
correct for any published snapshot **as long as a field's function signature can
never change in place**. Two rules enforce that:

- Detaching a BM25 or MinHash function (keeping its output fields) is rejected
  at both proxy and rootcoord — on the new `AlterCollectionSchema` path AND the
  legacy `AddCollectionFunction`/`AlterCollectionFunction`/`DropCollectionFunction`
  APIs: a detach or in-place parameter change would let query-time
  hashing/scoring silently mismatch data indexed under the old signature.
  Removing these functions always drops the output fields too, so a re-added
  function gets fresh field IDs.
- The search path carries a dropped-anns-field guard: a version-gated request
  whose version the ready snapshot already covers, targeting a field ID absent
  from the snapshot, is rejected with a clear input error (O(1) set lookup) —
  drops are the non-additive exception to "older versions are always
  serveable", and forwarding such a request would fail opaquely downstream (or
  silently mis-execute a BM25/MinHash text search) and blacklist a healthy
  delegator in the proxy LB. Version-0 legacy requests skip the guard.
- When the function was dropped in an applied-but-not-yet-published version, the
  latest runner version no longer maps the field; the read (which legitimately
  passed the version gate against the still-published old snapshot) gets a
  retriable `ErrCollectionSchemaVersionNotReady` rather than a hard
  ServiceInternal — transitional by construction, converging once the newer
  version publishes.

### Failure semantics (convergent retry, not physical rollback)

- **Load/reopen failure**: `LoadSegments` returns the error; querycoord's checker regenerates
  the task. Delegator state is convergent (idempotent `PutOrRef`, singleflight +
  missing-field-only stats downloads, monotonic gated publish). A failure never produces
  read-visible state: applied may have advanced, published has not. Pending marks are cleared
  on failure so an abandoned load degrades to progressive visibility instead of blocking
  publishes forever.
- **UpdateSchema failure**: blocked retry then panic + replay (above).
- **Runner init failure** (e.g. remote embedding endpoint down): `tryPublishReadySchema`
  errors → the surrounding retry loop (pipeline / load / DDL callback) re-drives it; reads
  stay on the previous version, never on a half-initialized one.

### Rolling-upgrade contract

This feature has NO "rolling upgrade into it" guarantee for schema DDLs, by
design. Three legacy escape hatches exist so that STEADY-STATE traffic survives
a mixed-version window: version-0 read requests skip the version gate,
version-less inserts skip the streamingnode exact-match gate, and pre-upgrade
querynodes report no ready version (treated as ready by the handshake). They
protect existing traffic during the upgrade; they deliberately do NOT make
schema DDLs safe in that window — an old component cannot honor semantics it
does not know. The operational contract is therefore: upgrade order
coordinator → querynode → proxy → SDK, and NO schema-change DDLs while the
cluster is mixed-version. Closing the hatches (or auto-rejecting schema DDLs
under a cluster-min-version check) is possible follow-up hardening, but the
contract, not the hatches, is the safety boundary.

## Invariants

1. Reads are served only from a published snapshot whose runners are initialized and whose
   loaded BM25 stats are fully activated.
2. Queries compiled against version `v` never fail due to a schema change to `v' > v`
   (old snapshot keeps serving; segcore staged reopen keeps segments servable throughout).
3. `AddCollectionField` returning success ⇒ every loaded shard serves the new version for
   reads and writes. Coverage of *pre-existing* rows under the new field follows backfill
   progress (independent job), not the DDL.
4. A schema-change WAL message is applied exactly once per delegator, never skipped.

## Alternatives considered

- **Per-request field-ID dependency validation** (previous revision): allowed unrelated reads
  through during catch-up but required plan unmarshal per request, missed semantic-only
  changes (e.g. analyzer params), and needed fragile expr traversal. Replaced by the version
  gate; the handshake makes gate rejections rare in practice.
- **`deferPublish`/`resumePublish` (control-flow deferral)**: required every publisher to
  coordinate ordering and was unwired in practice; replaced by the state-derived checklist.
- **Gating the DDL on backfill completion**: circular (backfill needs the committed DDL);
  rejected in favor of progressive visibility.

---

## Part II — Dropped-Field Tombstones: Reject Silent `$meta` Fallback After DropField

### Problem

`DropField` removes the field from the live schema outright (hard removal:
`buildSchemaForDropField`, index cascade, segcore filtering, async physical rewrite by
the schema-bump compactor). After the drop, any *explicit reference* to the dropped
name behaves inconsistently depending on the collection:

- **Non-dynamic collections**: reads and writes fail with a generic
  `fieldName not exist in collection schema` — correct but uninformative.
- **Dynamic-field collections**: the name silently falls back to `$meta`:
  column-based writes resolve through `GetFieldFromNameDefaultJSON` to the dynamic
  JSON field; row-based writes are packed into `$meta` by the SDK and the proxy's
  key scan only checks **live** static names; filter expressions and output fields
  resolve to `$meta["name"]`. Result: **no error anywhere** — new values land in
  `$meta.<name>` while historical values (in the dropped physical column) are
  invisible and eventually compacted away. The logical field's data is silently
  fragmented across two locations; this is data corruption without any failure
  signal, discovered only much later.

The proto enum `FieldState` (`FieldDropping`/`FieldDropped`) was evidently designed
for tombstoning but is unused by the current drop path.

### Semantics

Any **explicit** reference to a dropped column fails loudly, symmetrically for reads
and writes, on all collections (dynamic or not):

| Operation | Behavior |
|---|---|
| insert/upsert with the dropped column (column-based) | rejected |
| insert/upsert whose `$meta` JSON contains the dropped name as a key (row-based) | rejected |
| search/query/delete filter expression referencing the name | rejected at plan compile |
| explicit `output_fields` containing the name | rejected |
| `select *` / output `*` | **succeeds**; expands live schema only, never includes the dropped column |

Errors are `ParameterInvalid` (InputError, non-retriable — the request content is
what forces the branch) with an actionable message naming the dropped-at version.

### Design

**Storage — first-class schema field.** A `repeated DroppedFieldInfo dropped_fields`
on `schemapb.CollectionSchema` (milvus-proto `schema.proto`) holds
`{name, fieldID, dropped_at_version, sub_field}` per tombstone. Because it lives on
the schema itself, it rides the existing alter-collection broadcast and schema
version bump exactly as the field drop does — same propagation, cache-expiry, and
readiness (Part I) guarantees — and persists through the same etcd path as the
other scalar schema fields (`version`, `enable_dynamic_field`, `external_source`):
carried on `etcdpb.CollectionInfo.Schema`, not stripped out with `Fields[]`. The
rootcoord builders that already rewrite the schema on drop set it; the proxy reads
it directly off the schema it caches (`SchemaHelper`), no JSON decode. It is
server-maintained: no public API writes it (drops/adds are the only mutators), so
unlike `max_field_id` it needs no property-immutability guard — it is simply
unreachable through the property API by construction.

(An earlier revision carried the tombstones as a JSON-encoded collection property
`dropped_fields`; this is the promoted typed form, and the JSON encoding is gone.)

**Scope.** Top-level field and function-output-field drops write full tombstones:
the name was both a resolvable identifier and blocked as a `$meta` key while live,
so both reads and dynamic writes are rejected. Struct-array field drops write
`SubField`-scoped tombstones for every sub-field name: sub-field names were
resolvable identifiers (`SchemaHelper.nameOffset` includes them), so without a
tombstone an expression referencing one would silently fall back to a `$meta`
lookup — but they never participated in the top-level `$meta` key namespace (the
static-name collision check scans `schema.Fields` only), so the `SubField` scope
blocks name resolution only and `$meta` writes under those names stay legal,
exactly as they were while the struct existed. The struct's own name gets no
tombstone (never a resolvable identifier, `$meta` usage always legal).

**Lifecycle.** Created on DropField (full scope; `SubField` scope for struct
sub-fields) and drop-function-field. Clearing and shadowing are BOTH
namespace-scoped, symmetrically: a FULL tombstone clears (and is shadowed) only
when a TOP-LEVEL field re-uses the name — via the `AddCollectionField` RPC or
the `AlterCollectionSchema` add request; a struct or struct sub-field re-using
the name does NOT clear or shadow it, because the dropped column's physical
data still exists and its `$meta` usage stays hazardous. A `SubField` tombstone
clears when `AddCollectionStructField` re-uses the sub-field name (in both
stored forms, legacy plain and prefixed) and is shadowed by any resolvable live
name. `dropped_fields` is server-maintained and unreachable through the public
property API by construction (it is a schema field, not a property); the related
`max_field_id` property stays immutable through that API (both SET and delete
rejected), because overwriting or deleting it would let a dropped field's ID be
re-assigned and resurrect not-yet-compacted old column data. No auto-TTL and no
user-reachable purge today; an explicit purge API is follow-up work.

**Check sites (proxy only; resolution-miss cold paths).** Tombstones are consulted
only when a name fails to resolve against the live schema — hot-path traffic pays
nothing. Three of the four sites collapse into one chokepoint:
`SchemaHelper.GetFieldFromNameDefaultJSON`'s JSON-fallback branch (column-based
insert validation, plan-parser identifier resolution, output-field translation all
resolve through it); `SchemaHelper` parses tombstones once at construction, same
pattern as the timezone property. The fourth site is `verifyDynamicFieldData`'s
existing per-row `$meta` key scan (row-based writes), which checks non-`SubField`
tombstones in both the strict and the partial-update variants — an incoming dropped
key is never legitimate.

**Pre-existing `$meta` data.** A key may legitimately exist in `$meta` from before
the same-named static column was ever added; with tombstones those old dynamic
values become unreachable by name until purged. Also, on dynamic collections
`select *` returns the whole `$meta` content, so same-named keys that leaked in
before this feature remain visible there — tombstones prevent new leakage, they do
not scrub history (read-side `$meta` filtering was rejected: per-row JSON rewriting
on the read hot path).

### Out of scope, by construction

- **Legacy version-less writers** (rolling-upgrade window): their inserts bypass
  the streamingnode version gate and are already in the WAL; consumption must never
  fail (Part I's never-skip rule), so the dropped column's data is silently
  discarded by segcore. Only operational discipline covers this.
- **In-flight old-version reads during the drop transition**: covered by Part I's
  version gate, retriable runner-transition NotReady, and the version-scoped BM25
  guard in `prepareSearchFunction`.

### Invariants (Part II)

1. A full-scope tombstoned name absent from the live schema is neither readable nor
   writable through any API path — including via `$meta` — on any collection type.
   A `SubField`-scoped name is not resolvable as an identifier, while its `$meta`
   usage stays exactly as legal as it was while the struct existed.
2. `select *` output never contains a dropped column.
3. Re-adding the name restores full use under a **new** fieldID and clears the
   tombstone in every add path; historical data of the old fieldID never resurfaces.
4. Tombstone visibility is exactly as consistent as the drop itself: both travel in
   the same schema version bump through the same broadcast and readiness handshake.

### Alternatives considered (Part II)

- **Reuse `FieldState_FieldDropped` inside `schema.Fields`**: every consumer of
  `schema.Fields` would need to filter dropped entries; one missed site is a new
  bug class. Rejected for blast radius. (The chosen `dropped_fields` is a *separate*
  repeated field, so no `schema.Fields` consumer sees tombstones.)
- **SDK-side checks**: N SDKs, bypassable by raw gRPC, and the SDK cannot know drop
  history anyway. Rejected; proxy-side enforcement is authoritative.
- **JSON collection property (`common.DroppedFieldsKey`)**: avoided the cross-repo
  proto change but needed a decode on every `SchemaHelper` build, a property-
  immutability guard to keep users from clobbering it, and would have grown a
  migration when eventually promoted. Superseded by the typed `dropped_fields`
  schema field, which removes all three.
- **Auto-TTL for tombstones**: silently re-opens the hole exactly when everyone has
  forgotten about the drop. Rejected in favor of explicit purge.
