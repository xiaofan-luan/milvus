# Share immutable scalar-index validity across filter expressions

- Created: 2026-10-20
- Status: Draft
- Component: QueryNode scalar indexes and expression execution
- Tracking: [Milvus #54434](https://github.com/milvus-io/milvus/issues/54434)

## Problem

Indexed scalar expressions commonly cache a full-segment hit bitmap and an
owned validity bitmap before returning batches. For `L` evaluated leaves over
`N` rows, the validity copies alone retain approximately `L * ceil(N / 8)`
bytes, even when every leaf reads the same sealed index. A non-nullable index
also materializes a full all-valid bitmap for each expression.

The hit bitmap depends on the predicate. Sealed index validity can be shared
because it is finalized before queries read it. This proposal removes repeated
full-segment validity materialization without changing the owned result
contract of `IsNotNull()`.

ARRAY contains compilation merging is a separate optimization implemented in
[Milvus #52138](https://github.com/milvus-io/milvus/pull/52138). It can reduce the
number of hit bitmaps, while this change independently reduces validity copies
for predicates that remain separate.

## Optional borrowed validity view

`INullReader::GetValidityView() const` returns `std::optional<BitsetView>` using
Milvus's existing read-only view. No new owning bitmap wrapper is introduced.
`nullopt` means unsupported. An engaged view with `size()==0` means every
coordinate is valid, with the logical length supplied separately by `Count()`.
A nonempty view is dense and unmapped, has non-null data, and its size equals
the reader's coordinate count. The view is a validity mask (one means valid).

The sealed inverted reader retains its ordinary, by-value `TargetBitmap` and
returns a borrowed view into it. The all-valid case allocates no full bitmap.
Expression-selected readers already remain alive through `selected_root_pin_`
(and the selected JSON view/root pin). A returned view may only be read while
that pin remains alive. Views are cleared before index selection is released;
they do not extend lifetime independently and must not survive index eviction
without a pin. No view is published through the expression-result cache.

Unsupported readers still call the existing virtual `IsNotNull()`. The expression
owns that fallback bitmap and borrows a view of it. Its logical length comes
from the returned bitmap, not a second reader count that could change during
concurrent insertion. `IsNotNull()` itself keeps its owned/writable contract.

Knowhere view construction does not scan the bitmap. This API uses only
`size`, `data`, and `test`. Offset gathers touch only the requested coordinates;
they do not scan the full bitmap to discover all-valid input. The reader's
zero-size sentinel already provides that information without a scan.

## Expression ownership and writes

Expression-private validity uses one state with mutually exclusive forms:

- a borrowed reader view together with its logical coordinate count;
- an owned snapshot, whose logical length and read view derive from that owner;
- an uninitialized state.

Assignments replace the representation. Copies of owned fallback states retain
their owner, without retaining a separate raw descriptor into it. This source
state exposes no writable owner or ownership-transfer operation. Borrowed states remain tied to the expression's existing
reader pin and are cleared before that pin is released. They are never published
as an independent result or expression-cache value.

Batches copy only the requested range into independent writable output with the
existing bit-copy primitive. All-valid output is extended with true bits. Non-aligned
prefixes and suffixes retain neighboring bits, and range checks use the logical
length, including the all-valid sentinel case.

Offset evaluation gathers directly from the state, including BinaryRange's
non-flat JSON index path. It retains the full hit bitmap needed by repeated
batches, but does not create an additional full-segment validity copy. Repeated
and unordered offsets retain their original order, and bounds use the logical
length even for an all-valid sentinel. JSON-flat path/type validity keeps its
existing computation and cache behavior.

The common index-result slicing path retains the existing whole-buffer move for
computed query-local bitmaps. Expression-cache reads decode independent bitmaps;
the result and validity can share a lifetime control block without sharing bytes,
so `use_count()` alone must not trigger a clone. Reader validity and fallback
snapshots are copied into output, never moved. Raw-data/statistics caches and
cache-enabled evaluation keep their existing owned representation and interfaces.
No output mutation can modify source validity.

## Boundary toward immutable filtering results

ColumnVector, logical combiners, FilterBitsNode, and MVCC still use their existing
mutable-output interfaces. This change only borrows index validity under the
expression's reader pin; it does not publish ownerless views across operators.

A later migration should publish read-only inputs with their owner and coordinate
domain, while keeping exclusive writable scratch/output buffers for three-valued
logic. The final query-owned mask can compute
`~(predicate_data & predicate_validity) | timestamp_excluded | delete_excluded`.
Shared delete snapshots must remain read-only. Iterative candidate filtering and
row/element conversion require separate output domains. Those interfaces belong
to a separate change.

## JSON and derived-reader semantics

Field validity, JSON path existence, and typed comparable-value masks are
different concepts. A derived reader may expose the capability only when its
view exactly matches its own `IsNotNull()` semantics.

The sealed inverted reader provides its own coordinate-domain
validity. JSON-flat comparable/path-specific readers retain their existing
owned results. Reader selection must preserve row-versus-element coordinates
and must not substitute field-level validity for path/type validity.
Typed JSON-path resolution may reuse an inverted reader whose existing
validity already represents the resolved path/type; its coordinate and row
count checks still apply.

## Expression-result cache compatibility

Master's expression-result cache currently owns a writable result/validity
pair. The initial implementation preserves that contract: paths participating
in that cache retain their existing owned validity materialization. Ordinary
uncached index evaluation, validity-only evaluation, and offset reads may use
borrowed validity views where their reader supports them.

Changing the cache value representation is a separate follow-up. This proposal
does not claim to remove validity copies from every cache-enabled execution.

## Compatibility

No protobuf, persisted index format, public API, or configuration migration is
required. Unsupported readers use the existing behavior. New native binaries
must be rebuilt together because this is an internal C++ interface change.

## Validation

- Audit every concrete provider and derived override of `IsNotNull()` to verify
  that shared storage has the same semantics as the owned result.
- Verify that many independent leaves borrow the same pinned storage and never call the
  full-segment owned path when the capability is supported.
- Retain the reader pin while reading borrowed views; release all views before
  dropping that pin. No view promises to outlive the reader.
- Mutate one returned batch and confirm neither the owner nor another batch
  changes. Cover non-aligned appends, partial final batches, arbitrary/repeated
  offsets, zero rows, nullable rows, and all-valid storage.
- Compare indexed and raw execution across multiple batches and NULL/ARRAY
  predicates. Exercise growing and JSON path/type fallback behavior.
- For master, test full-range materialization and expression-cache fallback,
  including subsequent reads that would expose moved-from shared storage.
- Replace borrowed, owned, all-valid and all-null states; test copied/moved
  fallback owners and invalid ranges. Check whole-range output mutation cannot
  affect a retained reader or expression-cache entry.
- Run focused native index/expression tests and record source revisions,
  dependency configuration, and any unexecuted integration coverage.

Copy counters and pinned-lifetime tests establish the eliminated validity copies.
They do not establish production peak-memory savings, latency, throughput, or
the cause of every OOM. Those outcomes require matched workload replay with
the same indexes, segment concurrency, and cache configuration.
