# WAL payload chunking: oversized records split at the storage layer

- Status: Implementing (this PR)
- Date: 2026-08-22
- Scope: `pkg/streaming/util/message/chunk.go`, `pkg/streaming/util/types/streaming_version.go`, `internal/streamingnode/server/wal/adaptor/{wal_adaptor,scanner_adaptor}.go`, `internal/streamingnode/server/flusher/flusherimpl/wal_flusher.go`, `internal/streamingnode/server/service/handler/producer/produce_server.go`, `internal/streamingnode/server/resource/`, `internal/streamingcoord/server/balancer/`, `internal/distributed/streaming/`, `internal/proxy/{task_insert_streaming,task_delete,task_upsert_streaming}.go`, `pkg/util/paramtable/service_param.go`
- Related: #52474 (woodpecker.maxMessageSize); supersedes the packing half of the `insert-repack-view-encoder` line of work

## 1. Problem

Pulsar and Kafka enforce hard caps on a single record: Pulsar rejects above
`pulsar.maxMessageSize` (default 2 MiB, and the broker's own cap applies
regardless), and Kafka above `message.max.bytes`. Woodpecker has no equivalent
single-entry hard cap; this PR adds `woodpecker.maxMessageSize` as a Milvus WAL
chunk threshold so Woodpecker records use the same bounded granularity. When a
record crosses an enforced backend cap, the failure surfaces as an append error that
`appendOneWithRetry` classifies as recoverable — an infinite backoff loop.
Nothing between the producer and the broker can shrink the message, so **one
oversized insert permanently stalls the pchannel's write path**.

The proxy's row packing targets the Pulsar-shaped threshold, but it budgets
entity bytes only: the final materialized record adds the streaming header,
properties, and cipher expansion on top, and only the partial-update-CAS path
re-validates and re-splits the built message. A normal insert whose final
envelope crosses the backend limit — envelope growth, cipher expansion, or any
drift between the Milvus config and the broker's — reaches the WAL as one
oversized record.

## 2. Design

**P1 — Chunk at the storage layer, below the interceptor chain.** The payload
is an opaque byte blob there: no protobuf is unmarshaled or re-marshaled. The
exact bytes the backend would have stored are sliced in place. Every
interceptor (txn, timetick, fencing) and every consumer above the WAL sees
complete messages only — chunking is invisible to them.

**P2 — The reader reassembles before any interpretation.** The scanner
adaptor feeds every incoming message through a `ChunkAssembler` at the head of
`handleUpstream`, before filtering, reordering, or the txn buffer. A chunk is
never a valid message body, so nothing downstream ever parses one.

**P3 — Chunks are self-describing; the log needs no contiguity.** Every
chunk carries the original message's time tick (unique per message on a
pchannel) plus its index/total markers, so packs may interleave freely with
any other traffic in the log — the consumer pairs chunks by time tick, not
by adjacency. No write-side coordination is added on a path where master
runs appends fully concurrently.

**P4 — The successful first-chunk attempt's message ID is the logical message
ID.** The append caller is acked with the ID returned by the successful append
of chunk 0. A backend may persist an earlier attempt and still return an error,
so the reader replaces a payload-identical duplicate slot with the later log
observation. The reassembled message therefore carries the same successful
chunk-0 ID whether it comes from WAB tailing or durable catch-up.
`LastConfirmedMessageID` remains conservative: it is derived from the logical
ID after every chunk of the run has already been persisted.

### 2.1 Chunk format

```
payload (bytes)  ──slice──▶  [ c0 | c1 | ... | cN-1 ]   each ≤ limit - reserve
chunk record     =  payload slice + FULL clone of the original properties
                    + _ci (0-based index) + _ct (total count)
reassembled      =  concat(slices), properties of c0 minus _ci/_ct, ID of c0
```

Every chunk carries the complete property set, not a skeleton: backends and
walimpls-level delivery filters make per-record decisions from properties, so
each chunk must be delivered exactly where the whole message would have been.
The reserve (`pulsar.messageReserveSize`, default 64 KiB) absorbs the
per-record envelope — properties clone, cipher metadata/expansion, broker
metadata — so a chunk record never crosses the backend cap.

`SplitIntoChunks` returns the message unchanged when the payload fits or the
budget is zero; `IsChunkedPayload` recognizes chunks by the `_ct` marker.

### 2.2 Write path

`appendWithOptionalChunking` first computes the backend payload budget. Records
that fit, and backends without a per-record cap, call the pre-change
`appendOneWithRetry` path directly without waiting for any feature version.
Only an oversized record checks the latest in-memory assignment snapshot. If
`StreamingVersionChunking` has not been published, it fails immediately with a
non-retryable streaming error and asks the caller to retry after the rolling
upgrade; it never waits and never sends an oversized unsplit record to the
backend. Once ready, the WAL caches that monotonic observation and runs
`SplitIntoChunks(msg, chunkPayloadSize())` → `appendOneWithRetry` per chunk →
return chunk 0's ID. There is no lock anywhere on this path: master runs
appends fully concurrently and that property is preserved for all traffic,
oversized included. A run that fails unrecoverably midway leaves the caller
un-acked with a partial run in the log; the reader never assembles it and the
scanner keeps it incomplete, while the client's retry writes a fresh run under
a newly assigned time tick.

`chunkPayloadSize()` = `WALMaxMessageSize(backend) - reserve`, served from a
1-second cache because `GetAsInt` on refreshable items is not per-append hot
path work. A downward live config refresh takes effect within that window.

`WALMaxMessageSize` is the single place a WAL name maps to its chunking limit:
Pulsar and Kafka return their backend-enforced limits, while Woodpecker returns
the Milvus-configured `woodpecker.maxMessageSize` threshold. **Anything else,
including RocksMQ, returns 0**, which disables chunking entirely: RocksMQ's page
size is not a per-entry cap, and its pre-chunking behavior (store the oversized
record as-is) is preserved.

### 2.3 Read path

`ChunkAssembler.Push(msg)` at the head of `handleUpstream`, pairing chunks
into per-time-tick runs (packs may interleave; §3):

| Input | State | Result |
|---|---|---|
| ordinary non-chunk message | state untouched | process normally |
| TimeTick T | discard incomplete runs at or below T | process normally |
| first chunk of an unseen time tick | open a run | swallow |
| chunk filling a missing slot of its run | buffer at its index | swallow; if all `_ct` slots filled → emit reassembled message |
| chunk duplicating an already-filled slot payload byte-for-byte | redelivery (persisted-but-unacked retry rewrites it under a new message ID) | replace the slot with the later observation, then swallow |
| malformed markers, same slot with different bytes, or total mismatch inside one time tick | corruption | fail the scanner; the flusher marks the current WAL unavailable, so recovery cannot advance its checkpoint and new writes are rejected |
| middle chunk of an unknown time tick | nothing joinable | swallow |

There is no count-based eviction. The number of concurrently open runs does
not prove that the oldest writer is dead; evicting it can silently lose a
message whose writer later persists the remaining chunks and is acknowledged.
The next TimeTick is a safe cleanup barrier, however: after observing T, no
live writer can still append chunks for a run whose time tick is at or below T.
The assembler discards those proven-orphan runs while retaining newer runs.

An interrupted run never completes on its own. It remains local until a
TimeTick proves it orphaned (or the scanner closes); the producer was never
successfully acknowledged, and a client retry uses a new timetick. All
scanner-adaptor consumers — flusher, catchup, replication — sit above the
assembler, so the replication path ships only reassembled messages.

### 2.4 Proxy side: size-based packing removed

Once the WAL layer owns oversized payloads, the proxy's size-driven packing
stops earning its keep and is removed:

- **Insert**: one message per (channel, partition) group. Existing
  `PrepareResultFieldData` and `AppendFieldData` helpers materialize the
  selected rows once into a preallocated `InsertRequest`, which then uses the
  standard protobuf builder. There are no entity-size estimates, envelope
  re-validation, or single-row rejection: chunking slices bytes without row
  semantics, so even one huge row rides through.
- **Delete**: one tombstone batch per hashed channel. This also eliminates a
  latent master bug where a first PK already over the limit produced an empty
  first `DeleteMsg` that was still allocated an ID and appended.
- **Partial-update CAS** survives everything by construction: it is written
  into the materialized `InsertRequest.Base.Properties` and marked on the
  message properties outside the body by the existing `AddPartialUpdateCAS`
  builder helper. Both ride along — body verbatim through slicing/reassembly,
  properties via the full per-chunk clone (§2.1).
- The proxy→SN transport is not the bottleneck: the streaming gRPC channel
  pins all four message-size caps at 256 MB (`streamingNode.grpc.*`,
  `configs/milvus.yaml`), far above any realistic record.

The accepted price is memory granularity: while assembling an N-byte logical
message, one scanner temporarily retains about N bytes across the physical
chunk payloads and allocates another contiguous N-byte reassembly buffer. Peak
assembly memory is therefore about 2N per scanner, before any additional
downstream protobuf decoding, and multiple scanners on the same pchannel can
multiply that transient cost. After reassembly returns, the chunk references
become reclaimable and the contiguous N-byte message remains as one unit for
the flusher or txn buffer. Capping it again would reintroduce size-based
packing somewhere; deliberately NOT done.

## 3. Concurrency & ordering

- **The timetick watermark is enforced by the ack machinery, not by write
  adjacency.** A TimeTick(ts=T) record asserts that every record carrying
  ts ≤ T is already durable; reorder-buffer release, checkpoint advance,
  txn commit visibility, and crash recovery all consume that assertion.
  The interceptor acknowledges a message only when its append has fully
  returned — for a chunked insert, when the WHOLE pack is persisted — and
  the sync operator publishes `ts = lastAllAcknowledgedTimestamp()`, the
  consecutive acknowledged prefix (its own comment: "some message sent
  operation is blocked, new TT cannot be pushed forward"). So TimeTick(T)
  can never enter the log while any ts ≤ T message is still mid-pack,
  regardless of how records interleave. No write-side lock is needed to
  protect it; this is why the design needs none.
- Consequently packs interleave freely with other traffic in the log, and
  the consumer pairs chunks by time tick + index/total markers instead of
  by position (§2.3). Within one run the chunks are still sent
  sequentially by a single goroutine, so backend per-producer FIFO keeps
  their relative order — though even that is only an optimization, not a
  correctness requirement of the assembler.
- Chunk appends complete before the logical ID is returned upward, so the
  timetick interceptor's `LastConfirmedMessageID` never advances past
  un-persisted chunks — it under-reports at worst, by the tail chunks of
  the last run, until the next timetick confirms them.
- The assembler is scanner-local and single-goroutine (upstream delivery is
  serialized per scanner); its keyed state machine needs no locking.

## 4. Version activation

Chunk records are readable only by StreamingNodes carrying this change. There
is no per-channel version or durable watermark that says whether a pchannel has
ever contained chunks, so the balance policy must not guess from channel
ownership or exclude otherwise healthy nodes.

Chunking uses the existing StreamingVersion state machine and role-session
resolver as a cluster barrier:

```text
streaming service is enabled
    -> all Proxy sessions satisfy the version-3 schema prerequisite
    -> all StreamingNode sessions support chunking
    -> persist StreamingVersionChunking (4)
    -> publish a new assignment revision
    -> oversized appends may create chunks
```

StreamingCoord starts one background activation watch inside the balancer's
lifetime. It reuses the existing role-session resolver/watch path and follows
the supported rollout order: first wait until at least one StreamingNode is
present and all StreamingNodes are at least 3.0.2, then wait until at least one
Proxy is present and all Proxies are at least 3.0.0-beta (the version-3 schema
prerequisite). It then publishes version 4 exactly once. There is no raw etcd
`Get` in the balance loop, no periodic polling, and neither startup nor
balancing waits for activation.

StreamingVersion is a cumulative capability watermark, but its intermediate
numbers do not need to be persisted one by one. Version 4 may therefore be
written directly over an earlier value after the underlying prerequisites of
both version 3 and version 4 have been verified. Later `>=` checks remain safe
because version 4 is published only when the capabilities it implies are
actually present.

`MarkStreamingVersion` writes the cumulative feature version to the existing
StreamingCoord catalog, advances the assignment revision, and broadcasts it.
Assignment discovery delivers the version to StreamingNodes; coordinator and
node restarts recover the same monotonic value through the existing catalog and
assignment paths in both distributed and standalone deployments.

The StreamingCoord balance policy remains unchanged and no per-channel metadata
is added. New binaries always know how to read chunks, but a WAL creates none
until an oversized append has observed version 4. That append performs a
one-shot read of the assignment client's existing in-memory snapshot. A lower
or unavailable version fails fast; a ready version closes the
publication-to-observation race without delaying ordinary writes or sending an
unsplit oversized record into the legacy infinite retry loop. Once observed,
the WAL's process-local capability only moves from false to true.

## 5. Alternatives rejected

- **Keeping proxy-side size splitting (status quo ante).** Splits are visible
  above the WAL: multiple messages per user insert complicate txn/timetick
  semantics, replication, and every interceptor — and the packing itself was
  the cost center (entity-size estimates that still missed the envelope,
  re-marshaling per split, a single-row rejection wall). This PR removes the
  need for those splits and keeps structural fanout on the standard protobuf
  path, using the existing preallocated row-selection helpers.
- **A custom direct protobuf encoder in Proxy.** This could avoid the
  intermediate fixed-width row copy, but it would add a second hand-written
  InsertRequest codec to the write path. The standard builder is simpler and
  keeps protobuf compatibility centralized.
- **Splitting inside the SN interceptor chain.** Each interceptor would
  observe partial messages, and the split would have to be undone before the
  chain's bookkeeping (timetick, txn state machine) anyway.
- **A synthetic transaction wrapping the chunks.** A ghost begin/commit pair
  around every chunked insert adds txn-state machinery and extra records to
  solve a pure transport problem.
- **Marker records (begin/end) instead of property markers.** Doubles the
  record count for a run; `_ci`/`_ct` on each record carry the same
  information.
- **Raising the backend limits.** Not always available: the Pulsar broker's
  `maxMessageSize` is a broker-side cap an operator may not control.
- **Capping single-message size at the proxy** (e.g. 256 MB guard): any cap
  reintroduces size-driven rejection or splitting somewhere; rejected in
  favor of passing client batches through whole (§2.4).
- **Persisting a per-channel chunk capability bit.** The bit would have to be
  committed before the first chunk but remain consistent with a failed append,
  ownership changes, and recovery. Reusing the cumulative StreamingVersion
  state machine avoids adding channel metadata or another state machine.

## 6. Accepted gaps

| # | Gap | Trigger | Impact | Why accepted |
|---|---|---|---|---|
| 1 | Downgrade after cluster activation | A StreamingNode without chunk support is introduced after `StreamingVersionChunking` (4) has been persisted | It cannot reassemble historical chunk records | StreamingVersion is monotonic; reintroducing an incompatible binary after activation is unsupported without a drain/watermark protocol |
| 2 | Unbounded single-message memory granularity | Client submits a very large batch | During assembly, about 2N transient bytes per scanner (N retained chunk payloads + one contiguous N-byte buffer), multiplied by concurrent scanners; the contiguous N-byte message then remains one flusher/txn-buffer unit | Accepted design decision (2026-08-22): any proxy-side cap reintroduces size-based packing; revisit only if real workloads show pressure |
| 3 | Unsupported cross-item size configuration | A non-default backend message limit is no larger than the configured/default reserve | The effective reserve can become zero, leaving no envelope headroom | This change guarantees the shipped defaults; validating every live cross-item combination is out of scope |

## 7. Rollout & rollback

This is an additive WAL property encoding (`_ci`/`_ct`) with no protobuf schema
change. New StreamingNodes can read old complete records and new chunk records;
old StreamingNodes cannot interpret chunk records. New configs
(`woodpecker.maxMessageSize`, `pulsar.messageReserveSize`) use safe shipped
defaults. Individual parse errors fall back, but non-default cross-item
combinations are not claimed safe (§6 gap 3).

The intended live-write rollout order is:

1. Upgrade StreamingCoord so it can persist and publish
   `StreamingVersionChunking`.
2. Upgrade every StreamingNode to 3.0.2 or newer. New nodes can already read
   chunks, while version 4 remains unpublished and chunk creation stays
   dormant.
3. Ensure every Proxy satisfies the existing version-3 schema prerequisite. On
   a 3.0.x cluster this is already true; on an older cluster, upgrade Proxy only
   after step 2. During the transition, oversized writes fail immediately and
   may be retried after activation, while ordinary writes continue on the old
   path.
4. StreamingCoord persists version 4 after both role prerequisites are true.
   Assignment discovery publishes the new revision, and subsequent oversized
   appends may then create chunks.

Upgrading Proxy before all StreamingNodes is unsupported: a new Proxy can
route one large logical record to a legacy node that cannot split or later
reassemble chunks. The gate deliberately stays cluster-wide and does not add
an assignment policy or per-channel state because the cumulative cluster
version and rollout order provide the required compatibility boundary.

Downgrading or reintroducing a legacy StreamingNode after
`StreamingVersionChunking` is persisted is unsupported because historical WAL
may already contain chunks.

## 8. Testing

- `pkg/streaming/util/message/chunk_test.go` (runs with `-tags dynamic,test`):
  round-trip split→assemble (properties, payload, first-chunk ID), exact
  boundary fit, empty payload, non-positive chunk size, assembler
  reassemble / pass-through / retain-incomplete-across-interleaving; 33
  interleaved live runs are all retained; a duplicate chunk-0 retry uses the
  later successful observation's ID and properties; malformed/corrupt markers
  fail explicitly, declared totals do not preallocate slots, and TimeTick
  barriers discard only proven-orphan runs.
- `internal/proxy/task_insert_streaming_test.go` (new): row-selection parity
  (one message, header rows, decoded body matches source rows exactly, source
  unmutated), CAS metadata preservation, and empty selection.
- `internal/proxy/task_upsert_test.go`: former split/rejection tests adapted
  to single-message semantics; malformed source columns fail alignment checks
  before materialization.
- UTF-8 ingress: V2 validates the raw JSON request body once before field
  handling; scalar arrays use the same gjson element parser as struct-array
  sub-fields. gRPC continues to rely on protobuf string validation.
- Cluster activation tests cover the StreamingVersion wiring: old or invalid
  Proxy or StreamingNode sessions keep chunk creation disabled, all underlying
  prerequisites allow a direct earlier-version-to-version-4 transition, an
  oversized WAL append fails fast below the gate, and restart recovers the
  enabled state without changing channel assignment policy.
