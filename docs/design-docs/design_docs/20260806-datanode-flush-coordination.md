# DataNode Flush Coordination

Status: implemented
Scope: `internal/flushcommon/writebuffer`, `internal/flushcommon/syncmgr`, `internal/datanode/importv2`

## Problem

The DataNode flush path grew two data sources — payload buffered locally, and rows
still pinned in a segcore growing segment — and each grew its own scheduling,
retry, completion and shutdown machinery. The two were near-copies that had
already drifted apart, and the segment state machine they shared had a transition
that could be issued optimistically and then undone, which no reader could reason
about locally.

This document states the model the two paths now share.

## Segment state machine

The state machine lives in the DataNode's metacache. DataCoord has its own,
smaller one that shares the `commonpb.SegmentState` enum but not its meaning —
nothing writes `Flushing` on the DataCoord side, and the checks that read it are
vestigial.

```
Growing ──seal──> Sealed ──claim──> Flushing ──commit──> Flushed ──> removed
   │                 │                  │
   └─────────────────┴──────────────────┴──> Dropped   (collection/partition drop)
```

| Transition | Trigger | Executed by | Driven by |
| --- | --- | --- | --- |
| → `Growing` | first insert, or recovery | `CreateNewGrowingSegment` | WAL |
| `Growing → Sealed` | in-band WAL FlushMessage | `ddNode` → `sealSegments` | WAL |
| `Sealed → Flushing` | timetick / memory eviction / resync | `getSyncTask` | local |
| `Flushing → Flushed` | task commit succeeded | `SyncTask.Commit` | local |
| `* → Dropped` | collection / partition drop | drop message | WAL |

The first two transitions are decided by WAL position; the last two by local
scheduling. **What data belongs to a segment is decided by the WAL; when it is
flushed is decided by the DataNode.**

### Why `Sealed` and `Flushing` are separate

They answer different questions:

- `Sealed` — does this segment still accept writes? **No.**
- `Flushing` — is this flush claimed, with its content fixed? **Yes.**

Collapsing them loses two properties: policy idempotency (every timetick would
re-issue a task for the same segment) and the ability to retry *the same flush*
rather than re-deciding what to flush.

### The claim is one-way

`getSyncTask` claims `Sealed → Flushing` before `yieldBuffer` transfers the
segment's buffered content to the task. Both operations run consecutively under
the same `writeBufferBase.mut` critical section. The seal has already fixed the
logical tail; the claim records that fact, and the immediately following yield
transfers ownership of that fixed tail to the task:

- The seal arrived **in band** on the same single-threaded flowgraph
  (`dmStreamNode → ddNode → writeNode`), so every row of a `Sealed` segment is
  already buffered, and no further row can be assigned to it. Whatever the task
  takes is the segment's tail.
- A task that fails to build, or fails to commit, leaves the segment in
  `Flushing`. `GetSealedSegmentsPolicy` selects `Flushing` segments ahead of
  `Sealed` ones, so the retry resumes **that** flush.

There is therefore no `Flushing → Sealed` rollback. Selection performs no state
change at all: it cannot know whether the segment's source can produce the flush
yet, so claiming there would mean claiming optimistically and undoing on failure —
and an undoable claim is one another path can observe half-done.

## Two flush sources, one coordination model

`metacache.FlushSourceMode` records which subsystem owns a segment's payload; the
decision is sticky for the segment's lifetime.

| | `FlushSourceWriteBuffer` | `FlushSourceGrowing` |
| --- | --- | --- |
| Data lives in | the DataNode's insert/delta buffers | a segcore growing segment |
| Task type | `SyncTask` | `GrowingSourceSyncTask` |
| Row ownership | the task **owns** the yielded payload | rows stay pinned until `CommitGrowingFlush` |
| Cost of a failed attempt | the task must be retained — the rows exist nowhere else | one round trip |
| Per-segment state | `writeBufferSyncQueue` | `growingSourceProgress` |

That ownership difference is the only reason the two paths differ in structure.
Everything below is shared, and lives in `flush_coordination.go`.

### `flushIntent` — one debt, two triggers

A segment that wanted a flush and did not get one carries a debt:

```go
type flushIntent struct {
    owes  bool
    since time.Time // zero: not rate-limited
}
```

- `want()` — record the debt without stamping `since`: a fresh debt is due
  immediately, and one already rate-limited stays rate-limited unchanged. Used
  when the segment-local owner/reorder gate defers a new task — what unblocks it
  is an entry completing, so no delay is added. Node-wide payload admission is
  separate and happens before task materialization.
- `attempted(now)` — an attempt was just made (a re-drive, or a failure — the
  failed attempt IS an attempt): the debt stays (only a completed task settles
  it) but the rate limit starts over. Every driver must call this; without it
  `since` keeps the FIRST failure's timestamp and `due()` is true on every
  timetick from then on — one attempt per interval degrades into a retry storm.
  Failure paths express "rate-limited debt" as `want()` + `attempted(now)`.
  Restamping is unconditional: cascading failure callbacks inside one round each
  move `since` by the milliseconds between them, costing at most one extra
  interval on one retry — accepted in exchange for a two-verb model with no
  transition-only stamping rule.
- `due(now, interval)` — the interval is applied at **drive** time, not stored, so
  a changed `dataNode.flushRetryInterval` takes effect on debts already
  outstanding.

Every driver reads the same debt — task completion, normal timeticks, the
`bufferManager` retry backstop ticker, and the drop wait's own retry arm. All go
through `due()`, so no path can jump a retry interval that a failure just imposed.

`growingSourceProgress.owesFlush` is a **different** bit: sticky, set at seal,
cleared only by a successful flush task. A segment can owe a flush while having no
outstanding attempt to make, and vice versa.

### Node-wide payload admission

Per-segment reorder windows do not bound a node with many active segments. The
sync manager therefore keeps a node-wide task admission capacity of
`maxParallel * 2` (with a minimum of four). The write buffer reserves one slot
**before** it transfers payload out of its buffers:

```text
reserve outside writeBufferBase.mut
→ lock and recheck channel/segment lifecycle
→ claim Sealed → Flushing when needed
→ yield payload and register its writeBufferSyncEntry
→ unlock and submit
```

Admission may block, so it cannot run while holding `writeBufferBase.mut`:
completion callbacks need that lock to settle older entries and release slots.
A write-buffer entry owns its slot for the materialized task's whole lifetime,
including retry backoff and re-submission; a retry reuses the same lease. Only
success, terminal abandonment, or close returns it. Otherwise quick storage
failures could park one payload per segment while recycling the dispatcher slot
to materialize more, recreating the unbounded-memory bug behind a different
queue. Growing-source tasks own no yielded row payload, so their reservation is
per attempt and is returned by the completion callback.

This is a task-count bound, matching the previous dispatcher semaphore. It is
not a byte quota; per-task size remains governed by the existing flush-size and
import-memory limits.

### Metric boundaries

The split dispatcher exposes both phase metrics and two compatibility
aggregates:

- `PrepareQueueDuration` — admitted until `Prepare` starts;
- `PrepareDuration` — serialization and object-storage materialization;
- `CommitWaitDuration` — successful `Prepare` until `Commit` starts (or the
  prepared task is aborted before Commit);
- `CommitDuration` — the complete task Commit phase, including
  metadata/manifest publication and source finalization when applicable;
- `QueueDuration` — `PrepareQueueDuration + CommitWaitDuration`;
- `ExecuteDuration` — `Prepare + Commit + completion callbacks`, excluding
  admission and queue waits.

The aggregate names are retained for dashboards, but their baseline is not
directly comparable with the old single-phase dispatcher: the old queue metric
ended when monolithic `Run` started, whereas the new aggregate includes both
queue boundaries.

Legacy DataNode write metrics keep one ownership rule across both source paths.
Rows, bytes and save latency are published when a **new physical write** finishes
in `Prepare`; replaying a committed manifest does not publish them again.
Flush/auto-flush operation success is published after `Commit`, and a canceled
attempt is lifecycle rather than a failure.

### Retry drive cannot depend on a blocked flowgraph

Retries are dual-driven. The fast path is the channel timetick: `BufferData`
runs on every msgpack, including pure-timetick ones, and calls `driveRetries`
before handling new data, so a segment's queue is replayed from its oldest
task. The backstop is a single `bufferManager` ticker (period derived from
`dataNode.flushRetryInterval`, clamped to [100ms, 1s] — the per-segment
interval is still applied at drive time) that sweeps every registered buffer
and drives its due retries under that buffer's own locking. It runs on its own
goroutine, so it keeps working when the flowgraph goroutine is parked —
waiting for node-wide admission or flush backpressure — or when the WAL is
simply idle. A full admission budget made entirely of parked retry entries
therefore cannot self-deadlock, and no wait has to pump retries in-line.

The one exception is `waitSyncsSettled`: `DropChannel`/`RemoveChannel` do
`GetAndRemove` **before** `Close`, so a dropping buffer has already left the
manager map and the backstop cannot reach it — the drop wait keeps its own
retry arm.

Node-wide admission waiting is a plain bounded blocking call (graceful-stop
timeout), so a Drop message queued on the same flowgraph goroutine cannot wait
forever behind an unrecoverable storage outage. On timeout no payload has been
yielded and no slot can leak — the sync manager's acquire releases the
semaphore itself when a successful acquire races cancellation — and the
segment remains eligible for a later policy round.

### Terminal release and shutdown

- `releaseTerminalSync` is the single definition of "this task is done for good":
  discard its metacache syncing counters, release its payload and any prepared
  storage handle. Both terminal paths call it.
- `waitSyncsSettled` waits for write-buffer entries **and** growing-source
  progress together, under one `growingFlushCancelGrace`. The growing side
  signals completion through the `growingSettled` generation channel rather than
  being polled; `ackGrowingSyncLocked`, `failGrowingSyncLocked` and
  `cancelGrowingSyncLocked` are the only ways to leave the in-flight set, so the
  broadcast cannot be forgotten.

The wait is bounded after cancellation. An already-started native write takes no
cancellation token, so waiting past the grace turns the caller's timeout into a
hang — and a `DropChannel` that never returns is strictly worse than one that
reports failure and lets WAL replay redo the work. Giving up the wait is not
giving up the work: the task keeps its segment pin and finishes in the background.

## The rule every failure path follows

Two rules, both learned from defects this change had to fix:

**A task returns what it reserved, and that return is derived from the task.**
`releaseTerminalSync` (write-buffer) and `settleFailedGrowingTaskLocked` (growing)
take only the task: its metacache syncing rows and its checkpoint candidate. They
run unconditionally, never under a lookup of state a concurrent teardown may have
already removed. The growing failure path used to sit inside
`if progress, exists := ...growingSourceProgress[id]`, so a callback landing after
`abortDrop` skipped both — leaving the segment's `syncingRows` inflated forever
and the channel checkpoint pinned behind a candidate nobody would remove.

**Cleanup hangs off the point state disappears, not off a path that happens to
reach it.** The L0 partition→segment mapping was cleared only in the `triggerSync`
loop inside `BufferData`, while the segment leaves the metacache in
`finishWriteBufferSync`. A segment flushed through any other path — the memory
watchdog's `EvictBuffer` — left a dead mapping behind: the next delete recreated a
buffer for an ID that no longer existed, every sync attempt died on
segment-not-found, and that buffer and the checkpoint it pins never moved again.
`rotateL0SegmentLocked` therefore retires the mapping at task construction,
before `yieldBuffer`, which closes the window in which later deletes could join
a segment whose previous payload has already moved into a task.

The same rule explains what `abortDrop` must NOT do. It declares its data
un-committed and promises the checkpoint stays pinned for WAL replay — so it may
not clear `buffers` or `growingSourceProgress`, which are two of the three
candidate sources `GetCheckpoint` pins on. With all three empty the checkpoint
falls back to the latest CONSUMED position, past data that was never written.

A retry must also see a quiet queue. `needsRetryLocked` requires that no entry is
still submitted: a re-drive replays the whole queue from its oldest task, and a
second submission against an already-aborted dispatcher key finishes
synchronously — so the terminal branch would `Abandon` a task whose first
`Prepare` is still writing, pulling the payload out from under the writer and
orphaning the native handle it assigns afterwards.

## The flush range is a pair of WAL positions

Two representations, one per side, deliberately asymmetric:

- **DataNode, DataCoord and recovery keep the full `MsgPosition`.** The MsgID
  is the only thing a WAL can seek by, so it is what gets persisted
  (`SaveBinlogPaths CheckPoints[].Position` → the segment's DML position) and
  what recovery resumes from.
- **The source side consumes only the position's timestamp projection.** The
  flush range handed to segcore is `(startTs, endTs]`, resolved inside segcore
  against the segment's own rows via `get_active_count(ts)` — the same
  `upper_bound` the query path uses for MVCC visibility, bounded by the
  acknowledged insert prefix.

The projection is sound only within one vchannel: there the TimeTick order is
monotonic and every message's timestamp is unique, so a position and its
timestamp select the same boundary. Timestamps from different physical
channels are NOT comparable, and nothing in this design compares them — every
fence, watermark (tsafe) and checkpoint named here lives on the one channel
the write buffer owns.

The two fences:

- the lower fence is the position the segment was last flushed through
  (`metacache.lastFlushPosition`, restored from the DML position on recovery);
- the upper fence is the newest pack recorded for the segment, and the task
  publishes exactly that position — the full MsgPosition, not merely its
  timestamp — as its checkpoint, unchanged.

No row count crosses the boundary in either direction. Row offsets exist only
inside segcore, and they share no origin with anything the DataNode can keep: a
restart rebuilds the segment from a WAL replay and its offsets start over at
zero. The previous protocol — the DataNode accumulating a `targetOffset` row
count and reconciling it against segcore's `AckedRowCount` — required two
independently-maintained counters to converge; a divergence (a dropped row, a
replay with a different origin) had no way to self-correct and stalled the
segment silently.

The range is `(startTs, endTs]`. `upper_bound` semantics make the boundary
exact: a whole insert request shares one timestamp, so a fence can never split
a request; a pack's rows are all `<=` its end position's timestamp and the next
pack's are strictly greater, so a fence can never split a pack. Consecutive
flushes therefore partition the rows — every row written once, none skipped
(asserted directly by `FlushGrowingSegmentPartitionsRowsAcrossAdjacentFences`).

Readiness is a raw watermark read, never a wait. The source exposes `TSafe()`
— the position its pipeline has fully consumed and applied — and the task
refuses to run while `TSafe() < endTs`, surfacing a retryable error that the
write buffer re-drives on a later timetick. It must not block: the flusher and
the source consume the same channel through the message dispatcher, whose sends
are sequential, so waiting on the source's progress can stall the very
consumption being waited for. And it must not use the delegator's `waitTSafe`,
whose external-table and `DowngradeTsafe` escape hatches report success without
the watermark having advanced — acceptable for serving a slightly stale read,
data loss for a flush. Behind is a normal outcome; nothing is lost while it
lasts, because unflushed packs keep `firstUncommittedPosition` pinned and the
channel checkpoint cannot advance past them.

A retry of a flush whose data landed but whose metadata ack did not
(`pendingCommitted`) replays the FROZEN attempt — its manifest, checkpoint, row
count and finalization flags — rather than re-deriving any of them from live
state, which by retry time may already include newer packs or a concurrent
seal or Drop. In particular, a Drop that arrives after a periodic T1 manifest
was written is not ORed into that replay: doing so would publish the T1-only
manifest as a drop and discard a later T2 tail. Drop is tracked as an independent
monotonic debt. The replay first settles exactly T1; its completion then builds a
new drop task from the remaining live progress, which covers T2 before removing
the segment.

The same rule covers a final-flush replay already constructed when Drop arrives.
Its commit preserves an existing `Dropped` metacache state instead of overwriting
it with `Flushed`, and its completion does not remove the segment. The outstanding
drop debt drives a separate drop task after the frozen replay settles. Each task
removes only its own checkpoint candidate, so the channel remains pinned at T1
and then T2 until both ranges are durably committed.

Drop does not paper over an unavailable source: `syncDropSegment` retries on
`errGrowingSourceUnavailable` until its context expires, then fails loudly and
leaves the checkpoint pinned.

A metadata-only terminal task still owns checkpoint debt. Once its data batches
are durable, `growingSourceProgress.checkpointPosition` pins the full
`lastFlushedPosition` (`MsgID` included) until the flush/drop metadata commits.
This position can deliberately be older than the control-message pack that
created the debt: the conservative fence prevents `GetCheckpoint` from falling
back to the latest consumed position and checkpointing that pack before its
terminal action is durable.

A final task can also release the real growing source before a concurrent Drop's
metadata-only follow-up is constructed. `GrowingSourceSyncTask.SourceFinalized`
is therefore set only when a non-nil source actually receives
`CommitGrowingFlush`. Progress keeps an explicit proof bit (so timestamp zero is
not confused with the uninitialized state) plus the highest such fence in
`sourceFinalizedThroughTs`. A zero-row terminal task may proceed without
reacquiring the source only when that proof already covers its checkpoint;
otherwise source unavailability remains a retryable, checkpoint-pinning failure.

### Release waits the flush out instead of retaining its source

Nothing keeps a growing segment alive past its release so an in-flight flush can
finish. The ordering runs the other way: the release side must not drop the
segment until no flush still needs it. In growing-source mode the segment is the
only copy of the unflushed rows, so a flush whose source is dropped mid-flight
can never be completed by anyone — `getSyncTask` fails with
`errGrowingSourceUnavailable` on every retry and the progress entry, deleted
only once its batches drain, pins the channel checkpoint forever.

The release-manual-flush prepare therefore fences admission, appends a
`ManualFlush` (whose timetick is the fence), and then blocks in
`WaitGrowingFlushDrained` until no segment on the channel still owes a
growing-source flush. Only after that does the querynode drop its growing
segments. It deliberately does not wait for the write buffer to consume up to
the fence first: the drain's predicate keeps reporting a growing-source
segment as owing until it is actually Flushed, so the drain waits the
ManualFlush out regardless, and a pre-wait only delayed releases on channels
that owed nothing.

The wait alone is not airtight: the WAL keeps accepting inserts, so a segment
created around the release could still be admitted to growing-source mode and
lose its only data copy when the release drops it. The release therefore fences
growing-source **admission** — and the fence must be raised BEFORE the
`ManualFlush` is appended, which is the whole correctness argument:

- Everything admitted before the fence was created by an insert already in the
  WAL. WAL timestamps are monotonic and the `ManualFlush` is appended
  afterwards, so its fence timestamp is above all of them and it seals every
  one. They flush, and the drain converges.
- Everything admitted after the fence is refused growing-source mode and
  buffers its rows in the write buffer, where they survive the release without
  the delegator.

Fencing any later — even between the append returning and the next statement —
leaves a window for a segment that is growing-source **and** unsealed by that
`ManualFlush`. Such a segment reads as owing a flush forever (still
`FlushSourceGrowing`, never `Flushed`), so the release blocks to its deadline.

Admission and the fence check share `writeBufferBase.mut`, so a segment admitted
before the fence always has its progress entry by the time the snapshot and the
drain read the map; the drain re-scans that whole map rather than the caller's
list. The fence records the newest provider registration token and reopens only
when a provider with a newer token registers — that is, the channel was
re-subscribed locally. An abandoned release therefore leaves the channel in
write-buffer mode until it is re-subscribed: safe, and the cost of not having a
rollback to get wrong.

The provider takes **no part** in the release. It exposes growing segments as
flush sources and nothing else: no permission state to publish, validate, clear
or roll back, and no fence of its own to wait out. It once waited for the
delegator to consume the fence, which only converted a retry into a block — a
flush that runs ahead of the delegator fails its own `TSafe` check and is
re-driven, and the drain keeps waiting either way. The cost of dropping it is up
to one `flushRetryInterval` of release latency when the delegator lags; the
benefit is one less fence to reason about on the release path. For the same
reason the provider counts no leases of its own: a flush holds its segment through
`PinIfNotReleased`, and `LocalSegment.Release` already blocks on that refcount,
so a second counter would only track the same window less reliably. The segment
object still records the highest committed terminal fence
(`RecordGrowingFlushCommit`) as the `SourceFinalized` proof.

The drain asks only whether a segment still needs its growing SOURCE, not
whether it still owes work: once a terminal task has committed and notified the
source (`sourceFinalized`, set at exactly one site on that task's success path),
whatever remains is a metadata-only replay that `getGrowingSourceSyncTask`
builds without reacquiring the source. Waiting for it would delay the release
for nothing.

There is deliberately no fast-fail for "something owes a flush but no provider
is registered". Reaching it needs a delegator torn down while its own release is
mid-prepare, and the channel teardown path (`UnsubDmChannel`) closes the
delegator after the prepare returns. An earlier version signalled it with
`ErrChannelNotAvailable`, which `UnsubDmChannel` classifies as structural, i.e.
as "no local write buffer can owe a flush" — the exact opposite of what the
signal meant. That state now degrades the honest way: the drain blocks, the
release times out and the coordinator retries.

Two invariants to preserve when touching any of this:

1. **Release safety is the drain plus the admission fence, and nothing else.**
   Bookkeeping that merely describes what those two already guarantee is not a
   second line of defence — it is state that can disagree with reality.
2. **A guard must close a hole, not narrow it.** The drain alone is narrowing
   (new debt can appear after it) and the fence alone is narrowing (old debt
   survives it); only their combination closes, and only because WAL timestamp
   monotonicity makes their boundaries meet exactly. Two narrowing mechanisms
   do not add up to a closing one, so anything that merely shrinks a window —
   an "am I still active?" flag ahead of a pin, a pre-check ahead of an
   unsynchronised action — belongs nowhere in this path.

`UnsubDmChannel` distinguishes why a prepare failed. Structural unavailability —
no streaming node or preparer in this process, the channel served by another
node, the WAL shutting down — means no local write buffer can be left owing a
growing-source flush (the feature is process-local, and a closing WAL's buffer
dies with it while the unadvanced checkpoint replays), so the unsubscribe
proceeds without the drain. A merely transient failure (service unavailable,
client closed, read-only WAL) on a channel with a registered growing-source
provider fails the unsubscribe instead: the local write buffer may be alive with
such a flush in flight, and the coordinator's retry performs the drain this
attempt could not.

The transient guard keys off the **provider registration alone**, never off the
node's local growing-segment snapshot. `GetGrowingFlushSource` answers
`GrowingSourcePending` for a segment the QueryNode has not materialised yet, and
the write buffer treats `Pending` as a sticky "choose growing source". So the
write buffer can already own progress for a segment that
`localGrowingSegmentIDs` does not see; conditioning the guard on a non-empty
snapshot would let exactly that segment be dropped. The snapshot is a log field,
not a predicate.

### Two teardown paths, two different protections

`UnsubDmChannel` is **not** the only way a growing segment is dropped.
`ReleaseSegments` with `DataScope_Streaming` (or `_All`) drops growing segments
of a channel that stays subscribed — issued by `ReleasePartitions` or a target
update, and reaching the segment manager through two entry points: the delegator
(`shardDelegator.ReleaseSegments`) and the direct worker call
(`QueryNode.ReleaseSegments`). It is reachable in normal operation, because a
position fence flushes the `(start, end]` prefix while the segment keeps taking
rows: "persisted prefix + live growing tail" is the steady state, not an edge
case, and dropping the tail strands the debt exactly as an unguarded unsubscribe
would.

The two paths need different mechanisms because the channel's fate differs:

| path | channel after | mechanism |
| --- | --- | --- |
| `UnsubDmChannel` | gone | fence admission, append `ManualFlush`, **block** on the drain; fail the RPC if it does not converge |
| `ReleaseSegments(Streaming)` | still subscribed and ingesting | check the debt of **those segment IDs**, nudge it with a `ManualFlush`, and return a **retryable error without removing anything**; never block |

Why the partial release must not reuse the channel-release mechanism:

- **No `FenceGrowingSourceAdmission`.** The fence lives on the write buffer, so
  it is channel-wide, and it reopens only when a *newer* provider registration
  appears — i.e. on re-subscription. Closing it to release one partition would
  degrade every surviving partition of a live channel to write-buffer mode until
  the channel is watched again.
- **No `WaitGrowingFlushDrained`.** The channel-release path can afford to block
  because the channel is going away; here the RPC must return, so the caller
  retries instead. By the retry the nudged flush has normally settled the debt
  and the removal proceeds.

The nudge is a plain `ManualFlush`, and it is collection-scoped even though the
check is segment-scoped. A caller cannot scope a `ManualFlush` to segment IDs:
`ManualFlushMessageHeader.SegmentIds` is written by the shard interceptor as the
*output* of `FlushAndFenceSegmentAllocUntil`, and the one genuinely
segment-scoped seal message (`FlushMessageV2`) is rejected by the interceptor
unless the shard manager has already marked that segment flushed — it is the
segment flush worker's message, not an external API. So the extra segments the
nudge seals are simply flushed early, which is what a user `Flush()` does anyway.

**The nudge is rate-limited per (collection, vchannel); the refusal is not.**
The retry that makes this design work is the segment checker re-deriving the
dist/target diff every `queryCoord.checkSegmentInterval` (3s), so a debt that
does not settle would otherwise append one collection-scoped ManualFlush — and
therefore one collection-wide `FlushAndFenceSegmentAllocUntil` — every 3s for as
long as the flush is stuck, i.e. exactly when storage is already unhealthy. One
nudge per `10 × dataNode.flushRetryInterval` (30s by default) is enough: the
first ManualFlush seals the segments, and from there the flush path re-drives
itself on `flushRetryInterval` without needing another seal. The repeat still
exists because a segment created after the previous ManualFlush is not sealed by
it, so a debt episode can outlive its nudge. When the limit suppresses the
append the check still reports `pending`, so the release stays refused —
suppressing the nudge must never turn into allowing the release. The
reservation is taken before the append and returned if the append fails, so one
failed append does not silence the next attempt for a whole interval, and the
limiter's map is pruned to the last two intervals so it stays bounded by
recently nudged channels rather than by every channel ever released.

The refusal itself is retried, not dropped: `ReleaseSegments` for
`DataScope_Streaming` is always issued with `NeedTransfer=true`
(`task/executor.go`), so it runs the delegator path, and the delegator guard
refuses before `RemoveDistributions` — leaving the segment in the leader view.
`SegmentChecker.getGrowingSegmentDiff` derives growing reduce tasks purely from
leader-view-minus-target, so the identical task is regenerated on the next
checker round. The failed task itself is not re-run in place (querycoord marks
it failed and removes it); the checker is the retry loop.

On the delegator path the check must run **before** `RemoveDistributions` and
`AddExcludedSegments`, not just before the worker call. Excluding a segment stops
the growing segment from ingesting further rows while the write buffer still
expects to pull them from it — worse than the removal it was meant to precede.
Structural unavailability short-circuits both paths to the pre-existing
behaviour: remove immediately, no error.

### Why the write buffer is the baseline, not the fallback

Growing-source flush is available only while a delegator for the vchannel is
serving **in the same process** as the WAL owner's flusher — the source registry
is a process singleton. The rows exist in exactly one place, that delegator's
growing segment, so whether the optimisation is usable is decided by LOAD state,
which the write path does not control:

- Collection not loaded: no delegator, no provider, every segment is
  `FlushSourceWriteBuffer`. Milvus accepts inserts into an unloaded collection,
  so this is a steady state carrying live traffic, not a degraded one.
- Collection loaded on the WAL owner: growing-source is available.

`querycoord` prefers to place the delegator on the WAL owner
(`assignChannelToWALLocatedFirstForNodeInfo`) but nothing enforces it, and
nothing stops a channel from being unsubscribed while its WAL — and therefore
its write buffer — stays put. Release-collection, score balance, stopping
balance, RG reshuffle, manual transfer and repeated-channel eviction all produce
exactly that. Writes do not stop for any of them: no release path fences appends
(the ManualFlush's exclusive lock covers its own append and is released on
return), and fencing them for the duration of the drain would mean blocking
ingestion on every balance.

So the release faces two populations, and needs a different mechanism for each:

| population | mechanism | why not the other one |
| --- | --- | --- |
| already `FlushSourceGrowing` | **drain** — flush them out before the source goes | they cannot fall back: the mode is sticky and their earlier rows exist only in the growing segment |
| not yet seen / created after the fence | **admission fence** — make them choose the write buffer | they have no history, so their rows can simply be buffered instead |

The fence is not redundant with "the provider disappears": at fence time the
delegator is still alive and serving — `UnsubDmChannel` runs the whole prepare
BEFORE `delegator.Close()` — so without it a new segment would resolve `Usable`
and start owing a flush that the drain would then wait for, forever. This table
describes the CHANNEL release only; a partial `ReleaseSegments` has no second
population to handle (the channel keeps serving, so new segments stay
growing-source legitimately) and therefore uses no fence at all.

### What keeps a growing-source flush correct across a restart

A growing segment is rebuilt by WAL replay after any restart, and its row
offsets restart at zero. Nothing about the segment itself is ordered across
restarts. What holds instead is a chain:

1. A recorded-but-unflushed pack pins the channel checkpoint at its start
   position (`growingSourceProgress.checkpointPosition`), so the published
   checkpoint is never above the `startTs` of a pending range.
2. Both consumers resume from that same persisted checkpoint, inclusively — the
   flusher through `GetChannelRecoveryInfo`, the delegator through the
   querycoord target, both sourced from `meta.GetChannelCheckpoint`.
3. Both suppress the same already-flushed prefix, keyed on the segment's
   `DmlPosition`, which is also what `flushFromTs` is.
4. The range is resolved inside the segment by **timestamp**
   (`get_active_count(start_ts)` / `get_active_count(end_ts)`), never by offset,
   so a rebuilt segment resolves the same rows. One insert request carries one
   timestamp, so a fence cannot split a request.
5. A row-count cross-check refuses to publish metadata if the two sides ever
   disagree.

The head of a pending range can therefore never be missing: that would require
the persisted checkpoint to have advanced past it, which the pin forbids. Only
the TAIL can be missing — the delegator has not caught up yet — and that is what
`Pending` and the `TSafe < flushThroughTs` refusal exist for. Both leave the
checkpoint unadvanced and retry.

`Pending` means "behind but still consuming". Both halves are load-bearing: the
provider answers it only while it is serving, because a provider that has not
started or has been deactivated will never catch up, and the caller turns that
answer into a sticky, irreversible decision.

## Error classification

All classification is in `ClassifySyncError`; every layer below returns its error
unchanged.

| Decision | Meaning | Effect |
| --- | --- | --- |
| `SyncRetry` | default — throttling, coordinator not ready | keep payload, counters and queue position; arm the segment's intent |
| `SyncTerminal` | no attempt can change the outcome | `releaseTerminalSync`; checkpoint stays pinned for WAL replay |
| `SyncCanceled` | the caller went away | not a task failure, and never escalated |

`ErrSegmentNotFound` / `ErrChannelNotFound` are terminal. DataCoord saying the
target is gone cannot be fixed by trying again, and the meta writer already stops
its own retry loop for them — but the default is `SyncRetry`, so without an
explicit case the task would be re-driven on every timetick forever, pinning the
channel checkpoint behind a segment that no longer exists.

`SyncCanceled` must be excluded from fatal escalation: `DataNode.Stop()` closes
the sync manager **before** the flowgraphs, so during a graceful stop the
dispatcher aborts in-flight tasks with `context.Canceled` while the write buffer
is still open. Escalating that would make every drain-with-traffic panic.

## Storage-v3 manifest commit

The flush path commits with `packed.CommitManifestUpdates`
(`LOON_TRANSACTION_RESOLVE_OVERWRITE`).

`OverwriteResolver` applies the staged updates to a **deep copy of the manifest
that was read**, ignoring any newer one, and commits the result as `latest+1`. So
a version that appeared since — this handle's own lost answer, or a crashed
incarnation whose DataCoord ack never landed — is discarded rather than rebased
onto, and its files become orphans for object-storage GC. A retried commit
re-stages the same updates onto the same pristine base and never stacks them.

The pinning alternative (`RESOLVE_FAIL` on the read version) cannot express that:
it can only refuse. Refusing wedges the channel permanently, because the base
version comes from etcd, which the lost acknowledgement never advanced — every WAL
replay would reproduce the same refusal.

Single-writer-per-segment during flush is what makes discarding safe: the
dispatcher serializes commits per segment, and the stats tasks that share a
manifest only run once the segment is flushed.

## What is not accounted

BM25 stats are deliberately outside the write buffer's memory budget. They used
to be counted: folded into `insertBuffer.size`, handed to the task as payload,
and released by `Prepare` — while `SyncPack.ReleaseData` deliberately keeps them
alive until `Commit`. The accounting therefore claimed the memory was gone during
exactly the window it was still held, and `Abandon` did not clear them either.

They grow with distinct terms rather than with rows, so they are small next to
the row payload. Counting them consistently nowhere beats counting them wrong;
the alternative — a third accounting bucket released at Commit and Abandon — buys
precision the flush path does not need.

## Task ownership outside the write buffer

`SyncTask.Abandon` is the only way to release a task's payload and prepared
storage handle, and the write buffer calls it for tasks it owns. Import has no
write buffer, so every accepted import task installs `importv2.releaseOnDone` as
its dispatcher completion callback; the callback calls `Abandon` only after the
task has stopped running. `ImportTask.importFile` and `L0ImportTask.importL0`
still use `conc.BlockOnAll` as a deferred completion fence on every exit path.
Cleanup and waiting are separate responsibilities: the callback prevents
payload/native-handle retention, while the fence prevents the file worker from
returning its memory budget or publishing a terminal import state with a sync
still inside Prepare/Commit. `conc.AwaitAll` is the wrong primitive here because
it returns at the first failed future.

Import also has no single owner deciding a segment's physical layout. Concurrent
files can target the same segment, and a lazy Get-then-Add would let the second
initializer replace the first segment's manifest, statistics, row count, and
layout. `importv2.initImportSegments` therefore creates every request segment
before file workers start, and `NewSyncTask` refuses a missing segment instead of
initializing one. Each task then derives column groups from its own batch, uses
the metacache compare-and-set to agree on one winner, and freezes the winner on
the task. Writing files under two layouts and publishing one is an unrecoverable
column-group mismatch.

## Follow-ups

- DataCoord's `Flushing` state is dead: nothing writes it, and
  `handleFlushingSegments` can never find anything. Cleaning it up is independent
  of this change.
- `HandleLoonFFIResult` flattens every FFI failure into `ErrLoonTransient`,
  discarding the segcore code it already carries. Until that is fixed,
  `isLayoutMismatch` has to match on message text, and conflict cannot be
  distinguished from a transient error at the manifest layer.
