# Latency-weighted replica selection and hedged reads on 3.0

Related issue: [#55587](https://github.com/milvus-io/milvus/issues/55587).
This design adapts the prototype in
[#55588](https://github.com/milvus-io/milvus/pull/55588), authored by
xiaofan-luan, to the 3.0 delegator execution path.

## Scope and configuration

Replica selection policy is configured via `proxy.replicaSelectionPolicy`
(`round_robin`, `look_aside`, or `latency_weighted`; default `look_aside`).
`proxy.replicaSelectionPolicy` is refreshable (`refreshable: true`): live changes
apply to new channel requests, while an in-flight channel request retains the
policy and accounting owner captured at entry to `ExecuteWithRetry`.

Hedged reads are configured orthogonally under `proxy.hedging` (default
`enabled: false`) and apply to Search and Query (including Search requery and
query iterator reads) regardless of which `proxy.replicaSelectionPolicy` is
active. Each channel request sends at most one hedge across all retry rounds,
keeping speculative hedging orthogonal to `proxy.retryTimesOnReplica` error
retries. Other shardclient users use the configured selection policy without
hedging. No QueryNode protocol, persisted metadata, or WAL messages change.
Routing statistics are local to each Proxy and are not replicated between
clusters.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `proxy.replicaSelectionPolicy` | `look_aside` | Replica selection policy (`round_robin`, `look_aside`, `latency_weighted`); refreshable |
| `proxy.latencyWeighted.decay` | `10s` | Time constant for successful latency samples and penalties |
| `proxy.latencyWeighted.minShare` | `0.001` | Minimum selection probability among eligible replicas |
| `proxy.hedging.enabled` | `false` | Enable hedged Search/Query attempts across replicas, independent of `proxy.replicaSelectionPolicy` |
| `proxy.hedging.minDelay` | `200ms` | Minimum wait before looking for a hedge target |

Internally, `hedgeOutcomes` retains a `0.3` win-rate threshold (`defaultHedgeMinWinRate`) to
throttle ineffective hedges to a 10% probe rate.

## Selection and failure feedback

Replica observations are keyed by channel, request kind/consistency class, and
delegator node ID. The effective latency is the greater of the successful
time-decayed mean and the decaying failure penalty. Selection uses effective
latency (`effectiveLatency`) and the number of dispatched attempts still in
flight (`inFlight`):

```text
weight = 1 / (effectiveLatency × (inFlight + 1))
```

Candidates without recent observations use the mean of known candidates.
Every eligible candidate retains the configured minimum share. Topology,
resource-group scope and serviceability preference constrain candidates.
Existing unreachable-node marks constrain weighted selection. The existing
serviceable preferred-node fast path bypasses weighted selection and remains
unchanged.

Three consecutive failed attempts without an intervening success establish a
penalty equal to the attempt's available timeout, capped at one hour. Without
a caller deadline the assumed timeout is one minute. Both an execution error
and being overtaken by a successful hedge can provide failure evidence, but
one attempt contributes at most one failure. A late successful loser still
records its actual latency and resets the consecutive-failure count.

`attemptFeedback` serializes completion with overtake feedback. It records
whether an attempt has actually completed, rather than treating an unread
result-channel message as proof that the attempt still runs.

Input errors are ignored by replica learning and terminate retry. An actual
caller `context.Canceled` is also ignored. Deadline expiry and other system
errors remain failure observations. `ErrAttemptSuperseded` means that a
successful attempt lost result publication; it is observed as success and
consumed inside shardclient rather than returned as a wire error.

The weighted retry path uses request-local exclusions and decaying penalties.
It neither reads nor writes the legacy channel blacklist, so that blacklist
does not suppress recovery probes. Existing `look_aside` and `round_robin`
requests preserve the existing blacklist logic. Retrying starts after 10ms;
the shared retry options cap subsequent pauses at 20ms. The unscoped leader
count remains the retry budget, preserving 3.0's bounded polling behavior for
a resource group whose replicas are still becoming ready.

## Resource-group scope

`ExecuteWithRetry` applies the existing request-context scope before entering
either policy. `CollectionWorkLoad.ForChannel` carries the request class and
hedging capability alongside that scope, so namespace single-channel paths
and ordinary fan-out use the same selection inputs.

Primary selection, retries, and hedge target selection all filter candidates
by the request's resource group. A named group with no leader retains the
existing retriable `ErrCollectionNotFullyLoaded` result. A hedge cannot clear
exclusions or select the primary again. If only one eligible replica exists
inside the group, replicas in other groups do not make hedging possible.

Pooled request latency and hedge win-rate histories include the request scope
in their key. The unscoped request population has its own history. A slow or
overloaded group cannot thereby raise another group's hedge threshold or
suppress its hedges. Per-node successful latency, failure, and in-flight
observations remain shared when requests use the same channel/class/node.

## Hedged attempt lifecycle

The delay is the greater of the minimum delay and the recent p99 of the first
successful result in an attempt round. This is neither full client latency
nor the duration of slow losing attempts. The bounded sample ring holds up
to 1024 observations. It uses observations within ten seconds, retaining at
least the latest 100 when traffic is sparse. Samples are copied while holding
the balancer mutex and sorted after releasing it.

When the delay expires, the main result loop starts an asynchronous candidate
lookup. The lookup may use the cache or wait for coordinator refresh, but it
never reserves workload and never reads the mutable exclusion set. The main
loop remains able to receive the primary result during this wait. On a
successful lookup, the main loop applies the current exclusions and scope,
selects the target, reserves in-flight workload, and dispatches at most one
hedge. If selection of an alternate cached candidate fails, one fresh lookup
is attempted. A single-replica snapshot does not force a refresh just to hedge.

The lookup has a round-local child context. Returning from the round cancels
the lookup, including when the primary fails fast and the normal retry loop
will start another round. A buffered result channel and cancellation-aware
send prevent a late lookup result from blocking or creating a late hedge.

Dispatched primary and hedge attempts keep the outer request context. The
winner does not immediately cancel the loser: QueryNode can merge Search
tasks under a leading task's context, so earlier cancellation requires a
separate cancellation-isolation change. A losing attempt can therefore keep
running while other shards or Search requery finish.

Search and Query call `CommitAttempt` before writing any successful result.
Only the winner publishes its result and associated metadata. This includes
Search's serving-node map and Query's `actualChannelsMvcc` map; a late success
cannot overwrite the snapshot used by later processing. The existing input
MVCC and requery semantics are unchanged.

A canceled QueryNode RPC does not invalidate the shard leader cache when its
caller context is actually canceled. Active-context transport errors,
deadlines, NotShardLeader, and collection-not-loaded failures retain their
existing invalidation behavior.

## Hedge admission and observability

The latest 128 hedge outcomes supply a ten-second win-rate window. With fewer
than ten recent outcomes, hedges are allowed. Below the minimum win rate,
10% of potential hedges remain as probes. This is feedback-based suppression,
not a hard concurrency limit or a guarantee against added load during a burst.

The new counter `milvus_proxy_replica_hedge_total` has Proxy node ID and
`decision` labels: `sent`, `won`, `skipped_low_win_rate`,
`skipped_no_replica`, and `skipped_inflight_limit`. It does not add channel,
collection, or resource-group labels.

## Verification and limits

Focused tests cover:

- Weighted selection, decay, probing, failure classification and recovery.
- Completion-before-overtake and overtake-before-late-success/error orderings.
- A blocked coordinator lookup while the primary succeeds or fails fast,
  including lookup cancellation and absence of late workload reservations.
- Enable-switch changes in both directions, stable release accounting and
  retention of legacy blacklist behavior while the weighted policy is off.
- Resource-group candidate filtering and independent pooled hedge histories.
- Actual Search/Query result publication, winning node/MVCC metadata, and
  cache invalidation for caller cancellation versus deadline/transport errors.
- QueryNode status classification surviving serialization and actual task wrapping.

The failure-path audit traces QueryNode `Search`/`Query` through
`searchChannel`/`queryChannel`, delegator readiness and `waitTSafe`,
`merr.Status`, the distributed QueryNode client, and the task callbacks.
Collection-not-loaded, channel-not-found/not-available and tSafe-stalled errors
are system errors; BM25 metric validation produces an input error.
`merr.Status` carries the input flag and `merr.Error` reconstructs it. The
distributed client converts retriable statuses to errors even with one attempt;
this preserves their system classification before the task observes them.
Caller cancellation is checked against the original attempt context, rather
than inferred from a remote gRPC cancellation code. Native execution errors
keep the existing classification; this change does not claim to repair every
upstream error-construction site.

The prototype's fault-injection numbers are not measurements of this port.
Production throughput, fault recovery, burst overload, multiple Proxies, and
more than two replicas require separate deployment measurements. Heavy and
light requests within one class/scope still share a hedge threshold. Proxies
learn independently, and a shared WAL/consistency bottleneck cannot be bypassed
merely by choosing a different QueryNode replica.

## Attempt errors and heartbeat health

An InputError prevents a new retry round but does not discard an already
running sibling. If either dispatched attempt succeeds, its result wins.
If both fail, the input error is retained even when the other error arrives
later. With only one dispatched attempt, an input error still returns
immediately. Result-size errors can vary with the serving node's limit or
snapshot; their input classification alone does not prove both attempts fail.

Weighted selection creates the heartbeat health record for each candidate.
If heartbeat cleanup removed a record, reinitialization preserves an
unreachable mark from the retained consecutive heartbeat failure count.
Healthy heartbeats or successful cost updates restore eligibility through
the existing health transitions. Minimum share does not bypass this check.

Regression tests exercise real Search/Query task callbacks and status
conversion, both error arrival orders, winner metadata, and no new retry after
all attempts fail. Heartbeat lifecycle tests exercise registration, the failure
threshold, cleanup, rediscovery, and recovery without manually seeding health
records. RPC outcomes are controlled in tests; no distributed configuration
rollout or snapshot-divergence reproduction is claimed.

## Overload protection

Replica selection and hedging route around a replica that is slow or down.
They do nothing for a request that already waits in a QueryNode read queue,
and under load above what the replicas get done every request waits there:
the queue is limited by task count (`queryNode.scheduler.unsolvedQueueSize`),
which says nothing about time, so the whole excess becomes queue time. Every
request gets slow, most run into their timeout, and the work done for them is
thrown away: with one of three replicas down and the others overloaded, 1% to
2% of the requests were answered (see Measurements).

**Queue wait limit.** `queryNode.scheduler.maxQueueWaitRatio` (default `0`,
off; intended `0.1`) limits how long a read task may wait in the read queue:

```
limit = min(maxQueueWait, maxQueueWaitRatio * time left until the deadline)
```

`queryNode.scheduler.maxQueueWait` defaults to `1s` and is also the limit of a
task without a deadline. A new task is judged by how long the task that leaves
the queue next has waited so far, a measured time: when that is more than half
of the new task's limit, the new task is refused at once with
`ErrServiceTooManyRequests`. A task that waited longer than its limit after
all is refused when it leaves the queue. A task whose deadline has passed
fails as expired, not as refused. The limit applies to every scheduling
policy, and to the tasks a streaming node runs for growing data, which go
through the same scheduler. The delegator stage itself (waiting for tsafe,
fan out, reduce) has no queue and is not covered.

**Not retriable inside Milvus.** `ErrServiceTooManyRequests` was a retriable
error, which the gRPC client of a component sends again, up to 10 times with
backoff. It is now not retriable: no component sends a refused request again,
on any path. This also holds for the other places that return this error (a
read queue full by task count, the per-user limit, the task queues and the
REST admission of the Proxy). A replica that refuses is not put on the
channel blacklist either, which a non-retriable error otherwise does: it
works, and is left out for the one request only. The client gets code 4 and
may send the request again; the server does not do it for the client.

**One extra attempt.** A search or query gets one extra attempt on another
replica, and one only. It is used by whichever comes first: the first replica
refuses the attempt (the extra attempt takes its place at once, without the
retry interval), or the hedge delay passes (the extra attempt is the hedge).
When the extra attempt is refused too, the request ends with the refusal. A
replica that fails otherwise (it is down, the connection breaks) does not use
up the extra attempt: the request moves on as before. This holds under every
replica selection policy.

Under `latency_weighted`, where the Proxy knows how the replicas do:

- the extra attempt goes to the best replica that is left, by the measure the
  shares are made of (latency times attempts in flight), not to one drawn by
  share;
- it goes there only if that replica is better than the one that refused,
  which counts with the latency of a refusal at least (below). When the
  replica that refused is still the best there is, the others would take
  longer or refuse as well, and the request ends at once;
- a replica whose latest attempts all failed (three in a row, the last within
  the decay time) is not asked: the request would end with the failure of
  that replica, after the time it takes to fail, instead of the refusal.

**A refusal counts as latency, not as a failure.** A refusal says that the
replica has more work than it gets done, not that it fails: counted as a
failure, its penalty (the request timeout) would move all the traffic to the
other replicas and overload them in turn. Left out of the statistics, a
refusal makes the replica look better the more it refuses: only the attempts
it lets in count, and a refused attempt returns within milliseconds and leaves
nothing in flight. A refusal is recorded as a latency sample of two queue wait
limits: more than an attempt that was let in, which waited for the limit at
most before it ran. The Proxy computes the limit as the QueryNode does, from
the same two settings. This holds as long as running the request takes less
than the limit; a request that takes longer counts more when it is answered
than when it is refused.

**A failing replica gets the minimum share.** A replica whose latest attempts
all failed gets `minShare` only, as long as another replica answers. Its
penalty alone does not keep the traffic away when the other replicas are
loaded: they have a queue of attempts in flight, while its attempts fail at
once and leave nothing in flight, which the share formula takes for spare
capacity. Measured without this rule, a dead replica got 12% to 24% of the
channel requests while the other two were overloaded.

**A failed shard stops the others.** A request over several shards fails with
its first failed shard; the attempts on the other shards are canceled then.

### Measurements

Three replicas, each one streaming node and one 2-core QueryNode, on a shared
and noisy host; one replica gets about 40 searches per second done. Open-loop
search load, 2s timeout, hedging on. "This" is `maxQueueWaitRatio` `0.1`,
three runs; "before" is the same build without the changes of this section,
two runs. The fault is a power loss of the QueryNode of one replica.

| Case | This: answered, p50 | Before: answered, p50 |
|---|---|---|
| 50/s, no fault | 100%, 0.03s, shares 35/32/33% | 100%, 0.03s |
| 170/s, no fault | 72% to 73%, 0.21s | 79% to 89%, 4s |
| 170/s plus 1.5 slow requests per second | normal 52% to 60%, 0.21s; slow 52% to 59% | normal 57% to 60%, 5.8s; slow 11% to 13% |
| 50/s, one replica down | 100% but for 2 requests in one run | 100% |
| 120/s, one replica down, 3s to 20s | 68% to 71%, 0.21s | 38% to 41%, 1.7s |
| 120/s, one replica down, 20s to 40s | 70% to 71%, 0.21s | 1% to 2% |

Refused requests return after about 0.02s. In the overload row without fault
"before" answers more: it refuses nothing, and deep queues let the QueryNode
merge more searches into one execution; the load generator also holds back
requests above 256 in flight, which favors it. These runs were made before
the last two rules of the list above (the comparison with the replica that
refused, and its use of the latency of a refusal) were final.

Known limits:

- In the 20s after a fault, until the coordinator takes the dead node out,
  the replica that lost it is not served; the capacity of the others bounds
  what is answered. In the first 3s some requests wait for the dead node until
  their timeout.
- When a node dies, segments it held are loaded on the nodes that are left,
  which takes CPU from their queries for a moment: with the limit on, some
  requests are refused then that would only have been slow without it.
- Not measured: requests with different timeouts in one queue, Strong
  consistency under overload, the limit under the other replica selection
  policies, a streaming node (delegator) lost under overload with three
  replicas, a replica that is slow but not down.
