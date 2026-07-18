# Per-(field, chunk) cache cells for storage-v3 column groups

- Status: implemented (segcore); coalesced-read hint depends on milvus-common
- Date: 2026-07-17

## 1. Problem

Filtered search on a 1B-row collection fails with `InsufficientResource`
(`[MCL] CacheSlot failed to reserve resource for cells ...`):

```
Filtered search -> PhyFilterBitsNode evaluates a scalar filter (no scalar index)
  -> must scan the raw column -> pin its cache cell
  -> [MCL] CacheSlot reserves resource from the DList
  -> reservation returns EMPTY -> InsufficientResource -> the whole query fails
```

### Root cause

A storage-v3 column-group cache **cell was whole-group**: one cell held **all
fields** of a chunk (`GroupChunk` with every field). When a filter column
(e.g. `bool_active`) shares a physical column group with a large `ARRAY`
column:

1. Reading the filter column pins the whole-group cell -> loads **all** fields.
2. `has_array_field_` is decided per group -> the whole group's loading overhead
   is doubled (`loading_overhead_bytes = cell_size * 2`).
3. Reserving `~671MB data + ~1.31GB transient` fails on a memory-tight node ->
   `InsufficientResource`.

The filter column paid for the whole group plus the array field's 2x overhead.

## 2. Design

Change the cache cell granularity from **(chunk)** to **(field, chunk)**: one
cell holds exactly **one field** for one chunk range.

- `cid = field_index * num_chunks + chunk_index`
- `num_cells = num_fields * num_chunks`
- The chunk (row-group range) partition is **shared across all fields**, so a
  `chunk_index` maps to the same row range for every field.

Reading the filter column now pins only **its own** `(field, chunk)` cell:
- Only that column's bytes are read (projection reaches the cache/load layer).
- `loading_overhead_bytes` is decided **per field** — the array field's 2x stays
  on the array field's own cells, never contaminating siblings.
- The reserve for the filter column is small -> no more `InsufficientResource`.

### Cell type & the caching layer

The cell type stays `GroupChunk` (holding a single field), so
`CacheSlot<GroupChunk>` / `Translator<GroupChunk>` in **milvus-common** are
**not changed** and no conan bump is needed for the cell refactor.

### Dual-mode (keeps storage-v2 / JSON-key-stats intact)

`GroupCTMeta.num_chunks_ == 0` means **whole-group mode** (one cell per chunk
holds all fields), as produced by `GroupChunkTranslator` (storage v2) and the
JSON-key-stats index. Non-zero means **per-field mode** (`ManifestGroupTranslator`,
storage v3). `ChunkedColumnGroup::cid_of(field, chunk)` returns the chunk id in
whole-group mode and the `(field, chunk)` id in per-field mode — one code path
serves both, so storage-v2 and JSON-key-stats are untouched.

### reader_cg_index decoupling

The translator now holds the milvus-storage `Reader` and projects per field/
subset itself. Because JSON-key-stats builds a fresh single-group `Reader`
(index 0) while using the segment-level `column_group_id` for the cache key,
the translator takes both `column_group_index` (cache key / mmap path) and
`reader_cg_index` (index into `reader_` for `get_chunk_reader`).

### Sizing note

milvus-storage `get_chunk_size()` returns the row group's `total_byte_size`
(all columns) regardless of projection, so a per-column exact size is not
available. Per-(field,chunk) sizes are approximated as an even share of the
full row-group bytes (`total / num_fields`); the array field still gets 2x its
share. `num_chunks` uses the full-row-group size (a single reader), not the sum
across fields. A precise per-column size would need a milvus-storage API and is
left as a follow-up.

## 3. Coalesced reads

Per-field cells would read one field per IO. To read multiple columns of the
same group in one IO:

1. **Read (`ManifestGroupTranslator::get_cells`)** groups requested cids by
   chunk, and for each chunk reads the requested field-subset through one
   projected `ChunkReader` (`subset_chunk_reader`, cached by field-set), then
   splits the result into per-(field,chunk) cells. Warmup (all cells) is
   coalesced for free.
2. **Hint (`OpContext.coload_fields`, milvus-common)** carries the field ids the
   operation will read.
3. **Prefetch (`ChunkedColumnGroup::PrefetchColoadChunks`)** — on the expr's
   prefetch phase, `ProxyChunkColumn::PrefetchChunks` co-loads this field plus
   the hinted sibling fields of the same group in one `PinCells`, then releases
   the pins (cells stay cached). The subsequent per-field access hits warm cache.
4. **Set the hint (`segment_c.cpp`)** — search/retrieve set
   `op_ctx.coload_fields = plan->access_entries_`.

Correctness under concurrency comes from the caching layer's **per-cell
single-flight** (atomic loading claim + loading future): a given cell's bytes
are read exactly once no matter how many exprs/queries want it. Coalescing (one
IO for multiple columns) is guaranteed under sequential expr execution and
best-effort under a rare concurrent claim-split; a split never re-reads data,
it only misses a coalescing opportunity.

## 4. Dependency

`OpContext.coload_fields` lives in milvus-common:
**zilliztech/milvus-common#107**. This milvus PR bumps the milvus-common conan
pin to the version that includes it.

## 5. Verification

- Unit: `ManifestGroupTranslator*` / `ChunkedColumnGroup*` — 59 passed / 0
  failed, incl. `TestPerFieldCells` (each cell holds exactly one field).
- Read-path regression: `*Sealed*:*Retrieve*:*Expr*:*Search*:*ChunkedColumn*:
  *JsonKeyStats*:*GroupChunk*` — 5455 passed / 0 failed. Exercises the changed
  read call-sites through real segment reads/searches.
- Not yet covered: the end-to-end co-load-hint coalescing (needs an integration/
  benchmark or the 1B filtered-search scenario) and reproducing the reserve
  reduction on the production 1B collection.
