// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "segcore/storagev2translator/ManifestGroupTranslator.h"

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <exception>
#include <filesystem>
#include <limits>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <stdexcept>
#include <string>
#include <type_traits>
#include <unordered_map>
#include <utility>
#include <vector>

#include "NamedType/named_type_impl.hpp"
#include "arrow/api.h"
#include "cachinglayer/Utils.h"
#include "common/Chunk.h"
#include "common/ChunkWriter.h"
#include "common/Common.h"
#include "common/Consts.h"
#include "common/EasyAssert.h"
#include "common/FieldMeta.h"
#include "common/GroupChunk.h"
#include "common/Schema.h"
#include "common/Types.h"
#include "fmt/core.h"
#include "fmt/ranges.h"
#include "glog/logging.h"
#include "log/Log.h"
#include "milvus-storage/common/constants.h"
#include "milvus-storage/reader.h"
#include "segcore/Utils.h"
#include "segcore/memory_planner.h"
#include "storage/ThreadPools.h"
#include "segcore/storagev2translator/GroupCTMeta.h"
#include "storage/EntryStreamUtils.h"
#include "storage/Util.h"

#include <atomic>

namespace milvus::segcore::storagev2translator {

// See GroupChunkTranslator.cpp for explanation of g_mmap_path_generation.
static std::atomic<uint64_t> g_mmap_path_generation{0};

ManifestGroupTranslator::ManifestGroupTranslator(
    int64_t segment_id,
    GroupChunkType group_chunk_type,
    int64_t column_group_index,
    int64_t reader_cg_index,
    std::shared_ptr<milvus_storage::api::Reader> reader,
    std::vector<std::pair<FieldId, std::string>> field_columns,
    const std::unordered_map<FieldId, FieldMeta>& field_metas,
    bool use_mmap,
    bool mmap_populate,
    const std::string& mmap_dir_path,
    milvus::proto::common::LoadPriority load_priority,
    bool eager_load,
    const std::string& warmup_policy,
    const std::string& cache_key_suffix,
    int64_t fallback_bytes_per_row,
    std::string shard)
    : segment_id_(segment_id),
      group_chunk_type_(group_chunk_type),
      column_group_index_(column_group_index),
      reader_cg_index_(reader_cg_index),
      key_(cache_key_suffix.empty()
               ? fmt::format("seg_{}_cg_{}", segment_id, column_group_index)
               : fmt::format("seg_{}_cg_{}_{}",
                             segment_id,
                             column_group_index,
                             cache_key_suffix)),
      field_metas_(field_metas),
      reader_(std::move(reader)),
      meta_(field_columns.size(),
            use_mmap ? milvus::cachinglayer::StorageType::DISK
                     : milvus::cachinglayer::StorageType::MEMORY,
            milvus::cachinglayer::CellIdMappingMode::IDENTICAL,
            milvus::segcore::getCellDataType(
                /* is_vector */
                [&]() {
                    for (const auto& [fid, field_meta] : field_metas_) {
                        if (IsVectorDataType(field_meta.get_data_type())) {
                            return true;
                        }
                    }
                    return false;
                }(),
                /* is_index */ false),
            // Use getCacheWarmupPolicy to resolve: user setting > global config
            milvus::segcore::getCacheWarmupPolicy(
                warmup_policy,
                /* is_vector */
                [&]() {
                    for (const auto& [fid, field_meta] : field_metas_) {
                        if (IsVectorDataType(field_meta.get_data_type())) {
                            return true;
                        }
                    }
                    return false;
                }(),
                /* is_index */ false,
                /* in_load_list*/ eager_load),
            /* support_eviction */ true,
            std::move(shard)),
      use_mmap_(use_mmap),
      mmap_populate_(mmap_populate),
      mmap_dir_path_(mmap_dir_path),
      load_priority_(load_priority) {
    // ---- field ordering / reverse lookup / storage column names ----
    const size_t num_fields = field_columns.size();
    field_order_.reserve(num_fields);
    field_storage_columns_.reserve(num_fields);
    field_readers_.assign(num_fields, nullptr);
    for (size_t fi = 0; fi < num_fields; ++fi) {
        field_order_.push_back(field_columns[fi].first);
        field_storage_columns_.push_back(field_columns[fi].second);
        meta_.field_index_of_fid_[field_columns[fi].first.get()] = fi;
    }

    // ---- probe per-field, per-row-group sizes via single-column readers ----
    // All fields share the same row groups (same file), so the row-group
    // count and per-row-group row counts come from any field; the byte sizes
    // are per-field (each single-column ChunkReader reports its own column's
    // size). Building the per-field readers here also warms field_readers_ so
    // get_cells never has to build a reader on the hot path.
    std::vector<std::vector<int64_t>> per_field_rg_sizes(num_fields);
    std::vector<uint64_t> row_group_rows;
    size_t total_row_groups = 0;
    for (size_t fi = 0; fi < num_fields; ++fi) {
        auto cr = field_chunk_reader(fi);
        auto size_result = cr->get_chunk_size();
        if (!size_result.ok()) {
            throw std::runtime_error(
                fmt::format("get row group size failed for field {}: {}",
                            field_storage_columns_[fi],
                            size_result.status().ToString()));
        }
        const auto& sizes = size_result.ValueOrDie();
        per_field_rg_sizes[fi].assign(sizes.begin(), sizes.end());
        if (fi == 0) {
            total_row_groups = sizes.size();
            auto rows_result = cr->get_chunk_rows();
            if (!rows_result.ok()) {
                throw std::runtime_error(
                    fmt::format("get row group rows failed: {}",
                                rows_result.status().ToString()));
            }
            row_group_rows = rows_result.ValueOrDie();
        }
    }

    // ---- row-groups-per-chunk from the FULL row-group size ----
    // NOTE: milvus-storage get_chunk_size() returns each row group's
    // total_byte_size (ALL columns) regardless of the reader's column
    // projection, so every per-field reader reports the same full-row-group
    // size. Use one of them as the group total; summing across fields would
    // N-fold inflate it and produce N x too many chunks.
    std::vector<int64_t> group_rg_sizes =
        num_fields > 0 ? per_field_rg_sizes[0]
                       : std::vector<int64_t>(total_row_groups, 0);
    const int64_t cell_target_size_bytes = GetCellTargetSizeBytes();
    meta_.total_row_groups_ = total_row_groups;
    const size_t rgs_per_cell =
        ComputeRowGroupsPerCell(group_rg_sizes, cell_target_size_bytes);
    const size_t num_chunks =
        (total_row_groups + rgs_per_cell - 1) / rgs_per_cell;
    meta_.num_chunks_ = num_chunks;

    // ---- per-chunk row-group ranges + prefix-sum rows (field-independent) --
    meta_.cell_row_group_ranges_.reserve(num_chunks);
    meta_.num_rows_until_chunk_.reserve(num_chunks + 1);
    meta_.num_rows_until_chunk_.push_back(0);
    std::vector<int64_t> chunk_rows(num_chunks, 0);
    int64_t cumulative_rows = 0;
    for (size_t c = 0; c < num_chunks; ++c) {
        size_t start = c * rgs_per_cell;
        size_t end = std::min(start + rgs_per_cell, total_row_groups);
        meta_.cell_row_group_ranges_.push_back({start, end});
        int64_t rows = 0;
        for (size_t r = start; r < end; ++r) {
            rows += static_cast<int64_t>(row_group_rows[r]);
        }
        chunk_rows[c] = rows;
        cumulative_rows += rows;
        meta_.num_rows_until_chunk_.push_back(cumulative_rows);
    }

    // ---- per-field ARRAY flag (drives 2x overhead only on that field) ----
    meta_.field_is_array_.reserve(num_fields);
    for (size_t fi = 0; fi < num_fields; ++fi) {
        auto it = field_metas_.find(field_order_[fi]);
        bool is_array = it != field_metas_.end() &&
                        it->second.get_data_type() == DataType::ARRAY;
        meta_.field_is_array_.push_back(is_array ? 1 : 0);
    }

    // ---- per-(field,chunk) cell sizes, indexed by cid = fi*num_chunks + c --
    // milvus-storage cannot report per-column row-group sizes (get_chunk_size
    // is the full row group), so approximate each field's share of a chunk as
    // an even fraction of the full-row-group bytes over that chunk's range. A
    // precise per-field size would need a milvus-storage API exposing
    // per-column-chunk metadata. An array field still gets 2x its share via
    // loading_overhead_bytes.
    meta_.chunk_memory_size_.assign(num_fields * num_chunks, 0);
    int64_t last_resort_cells = 0;
    const int64_t nf = std::max<int64_t>(1, static_cast<int64_t>(num_fields));
    for (size_t fi = 0; fi < num_fields; ++fi) {
        const auto& s = per_field_rg_sizes[fi];
        for (size_t c = 0; c < num_chunks; ++c) {
            auto [start, end] = meta_.cell_row_group_ranges_[c];
            int64_t full_range_bytes = 0;
            for (size_t r = start; r < end && r < s.size(); ++r) {
                full_range_bytes += s[r];
            }
            int64_t cell_size = full_range_bytes / nf;  // even per-field share
            if (fallback_bytes_per_row > 0 && chunk_rows[c] > 0) {
                cell_size = (chunk_rows[c] * fallback_bytes_per_row) / nf;
            } else if (cell_size == 0 && chunk_rows[c] > 0) {
                constexpr int64_t kLastResortBytesPerRow = 4096;
                cell_size = chunk_rows[c] * kLastResortBytesPerRow / nf;
                ++last_resort_cells;
            }
            meta_.chunk_memory_size_[meta_.cid_of(fi, c)] = cell_size;
        }
    }
    if (last_resort_cells > 0) {
        LOG_WARN(
            "[StorageV2] translator {}: {}/{} cells had zero memory_size "
            "from format metadata and no sampled bytes_per_row; using "
            "4KB/row last-resort estimate",
            key_,
            last_resort_cells,
            num_fields * num_chunks);
    }

    LOG_INFO(
        "[StorageV2] translator {}: {} fields x {} chunks = {} cells "
        "({} row groups, cell_target_size_bytes={})",
        key_,
        num_fields,
        num_chunks,
        num_fields * num_chunks,
        total_row_groups,
        cell_target_size_bytes);

    // Set loading overhead config to cap total transient memory reservation.
    // Overhead is per-(field,chunk); use the max across cells (an array
    // field's cell contributes 2x its size).
    if (!meta_.chunk_memory_size_.empty()) {
        int64_t max_cell_sz = 0;
        int64_t max_overhead_size = 0;
        for (size_t cid = 0; cid < meta_.chunk_memory_size_.size(); ++cid) {
            int64_t sz = meta_.chunk_memory_size_[cid];
            max_cell_sz = std::max(max_cell_sz, sz);
            max_overhead_size = std::max(
                max_overhead_size,
                loading_overhead_bytes(meta_.field_index_of(cid), sz));
        }
        auto upper_bound = milvus::segcore::FieldDataLoadingOverheadUpperBound(
            max_overhead_size,
            use_mmap_ ? std::optional<int64_t>{max_cell_sz} : std::nullopt);
        // Keep MCL reservation aligned with the process-wide transient load
        // budget rather than multiplying it by translator type.
        auto group = milvus::segcore::kLoadTransientOverheadGroup;
        meta_.loading_overhead =
            milvus::cachinglayer::LoadingOverheadConfig{upper_bound, group};
    }
}

std::shared_ptr<milvus_storage::api::ChunkReader>
ManifestGroupTranslator::field_chunk_reader(size_t field_index) {
    if (field_readers_[field_index] != nullptr) {
        return field_readers_[field_index];
    }
    auto needed = std::make_shared<std::vector<std::string>>();
    needed->push_back(field_storage_columns_[field_index]);
    auto result = reader_->get_chunk_reader(reader_cg_index_, needed);
    AssertInfo(result.ok(),
               "[StorageV2] translator {} get_chunk_reader failed for field "
               "{} (col {}): {}",
               key_,
               field_index,
               field_storage_columns_[field_index],
               result.status().ToString());
    field_readers_[field_index] = std::move(result).ValueOrDie();
    return field_readers_[field_index];
}

std::shared_ptr<milvus_storage::api::ChunkReader>
ManifestGroupTranslator::subset_chunk_reader(
    const std::vector<size_t>& sorted_field_indices) {
    // Single-field subset reuses the per-field reader cache.
    if (sorted_field_indices.size() == 1) {
        return field_chunk_reader(sorted_field_indices[0]);
    }
    std::string cache_key;
    for (auto fi : sorted_field_indices) {
        cache_key += std::to_string(fi);
        cache_key += ',';
    }
    {
        std::lock_guard<std::mutex> lk(subset_readers_mu_);
        auto it = subset_readers_.find(cache_key);
        if (it != subset_readers_.end()) {
            return it->second;
        }
    }
    auto needed = std::make_shared<std::vector<std::string>>();
    needed->reserve(sorted_field_indices.size());
    for (auto fi : sorted_field_indices) {
        needed->push_back(field_storage_columns_[fi]);
    }
    auto result = reader_->get_chunk_reader(reader_cg_index_, needed);
    AssertInfo(result.ok(),
               "[StorageV2] translator {} get_chunk_reader failed for subset "
               "[{}]: {}",
               key_,
               cache_key,
               result.status().ToString());
    std::shared_ptr<milvus_storage::api::ChunkReader> reader =
        std::move(result).ValueOrDie();
    std::lock_guard<std::mutex> lk(subset_readers_mu_);
    // Another thread may have inserted concurrently; keep the first.
    auto [it, inserted] = subset_readers_.emplace(cache_key, reader);
    return it->second;
}

size_t
ManifestGroupTranslator::num_cells() const {
    return meta_.chunk_memory_size_.size();
}

milvus::cachinglayer::cid_t
ManifestGroupTranslator::cell_id_of(milvus::cachinglayer::uid_t uid) const {
    return uid;
}

std::pair<milvus::cachinglayer::ResourceUsage,
          milvus::cachinglayer::ResourceUsage>
ManifestGroupTranslator::estimated_byte_size_of_cell(
    milvus::cachinglayer::cid_t cid) const {
    assert(cid < meta_.chunk_memory_size_.size());
    auto cell_sz = meta_.chunk_memory_size_[cid];
    auto overhead_sz = loading_overhead_bytes(meta_.field_index_of(cid), cell_sz);

    if (use_mmap_) {
        return {{0, cell_sz}, {overhead_sz, cell_sz}};
    } else {
        return {{cell_sz, 0}, {overhead_sz, 0}};
    }
}

const std::string&
ManifestGroupTranslator::key() const {
    return key_;
}

std::vector<
    std::pair<milvus::cachinglayer::cid_t, std::unique_ptr<milvus::GroupChunk>>>
ManifestGroupTranslator::get_cells(
    milvus::OpContext* ctx,
    const std::vector<milvus::cachinglayer::cid_t>& cids) {
    // Check for cancellation before loading group chunks
    CheckCancellation(ctx, segment_id_, "ManifestGroupTranslator::get_cells()");

    std::vector<std::pair<milvus::cachinglayer::cid_t,
                          std::unique_ptr<milvus::GroupChunk>>>
        cells;
    cells.reserve(cids.size());
    if (cids.empty()) {
        return cells;
    }

    auto max_cid = *std::max_element(cids.begin(), cids.end());
    if (max_cid >= meta_.chunk_memory_size_.size()) {
        ThrowInfo(
            ErrorCode::UnexpectedError,
            "[StorageV2] translator {} cid {} is out of range. Total cells: {}",
            key_,
            max_cid,
            meta_.chunk_memory_size_.size());
    }

    // Coalesced read. Group requested cids by chunk, then by the field-subset
    // needed for that chunk, so all fields of a chunk are read in ONE projected
    // IO and split into per-(field,chunk) cells. A single field is the
    // degenerate subset {field}. cid = field_index * num_chunks + chunk_index.
    std::map<size_t /*chunk*/, std::set<size_t> /*field_indices*/>
        fields_by_chunk;
    for (auto cid : cids) {
        fields_by_chunk[meta_.chunk_index_of(cid)].insert(
            meta_.field_index_of(cid));
    }
    // Chunks that request the SAME field-subset share one reader and one
    // LoadCellBatchAsync (IO-merged). Key = sorted field indices.
    std::map<std::vector<size_t>, std::vector<size_t>> chunks_by_subset;
    for (auto& [chunk, fset] : fields_by_chunk) {
        std::vector<size_t> subset(fset.begin(), fset.end());  // set => sorted
        chunks_by_subset[subset].push_back(chunk);
    }

    std::unordered_map<milvus::cachinglayer::cid_t,
                       std::unique_ptr<milvus::GroupChunk>>
        completed_cells;
    completed_cells.reserve(cids.size());

    auto& pool = milvus::ThreadPools::GetThreadPool(
        milvus::PriorityForLoad(load_priority_));

    for (auto& [subset, chunks] : chunks_by_subset) {
        auto reader = subset_chunk_reader(subset);

        // One CellSpec per chunk (cid == chunk index). Size/overhead is summed
        // over the subset's fields for that chunk.
        std::vector<milvus::segcore::CellSpec> cell_specs;
        cell_specs.reserve(chunks.size());
        for (auto chunk : chunks) {
            auto [start, end] = meta_.cell_row_group_ranges_[chunk];
            int64_t sz = 0;
            int64_t overhead = 0;
            for (auto fi : subset) {
                auto cell_sz =
                    meta_.chunk_memory_size_[meta_.cid_of(fi, chunk)];
                sz += cell_sz;
                overhead += loading_overhead_bytes(fi, cell_sz);
            }
            cell_specs.push_back({static_cast<int64_t>(chunk),
                                  /*file_idx=*/0,
                                  static_cast<int64_t>(start),
                                  static_cast<int64_t>(end - start),
                                  sz,
                                  overhead});
        }

        auto factory = milvus::segcore::MakeChunkReaderFactory(reader);
        auto channel = std::make_shared<milvus::segcore::CellReaderChannel>(
            static_cast<size_t>(pool.GetMaxThreadNum() *
                                milvus::segcore::kChannelCapacityMultiplier));

        // finalize == nullptr: the batch returns raw Arrow tables (all subset
        // columns for the chunk); we split them into per-field cells below.
        auto load_futures = milvus::segcore::LoadCellBatchAsync(
            ctx,
            std::move(cell_specs),
            std::move(factory),
            channel,
            FieldDataLoadBatchSplitTargetBytes(),
            load_priority_,
            /*finalize_cell=*/nullptr);

        try {
            std::shared_ptr<milvus::segcore::CellLoadResult> cell_data;
            while (channel->pop(cell_data)) {
                try {
                    CheckCancellation(ctx,
                                      segment_id_,
                                      "ManifestGroupTranslator::get_cells()");
                    auto chunk = static_cast<size_t>(cell_data->cid);
                    // Split this chunk's subset tables into per-field cells;
                    // must run before releasing the budget (which clears
                    // cell_data->tables).
                    for (auto fi : subset) {
                        auto cell_cid = meta_.cid_of(fi, chunk);
                        completed_cells[cell_cid] = load_group_chunk(
                            cell_data->tables,
                            static_cast<milvus::cachinglayer::cid_t>(cell_cid),
                            field_order_[fi]);
                    }
                    milvus::segcore::ReleaseCellLoadResultBudget(cell_data);
                } catch (...) {
                    milvus::segcore::ReleaseCellLoadResultBudget(cell_data);
                    throw;
                }
            }
        } catch (...) {
            // Drain the channel to unblock producers stuck on push() to a full
            // bounded channel; otherwise their task_guard (which closes the
            // channel) never runs.
            std::shared_ptr<milvus::segcore::CellLoadResult> discard;
            try {
                while (channel->pop(discard)) {
                    milvus::segcore::ReleaseCellLoadResultBudget(discard);
                }
            } catch (...) {
                LOG_WARN("drain channel exception swallowed");
            }
            try {
                storage::WaitAllFutures(load_futures);
            } catch (const std::exception& e) {
                LOG_WARN(
                    "[StorageV2] translator {} cleanup ignored background load "
                    "exception after cancellation: {}",
                    key_,
                    e.what());
            } catch (...) {
                LOG_WARN(
                    "[StorageV2] translator {} cleanup ignored unknown "
                    "background load exception after cancellation",
                    key_);
            }
            throw;
        }

        storage::WaitAllFutures(load_futures);
    }

    for (auto cid : cids) {
        auto it = completed_cells.find(cid);
        AssertInfo(
            it != completed_cells.end(),
            fmt::format(
                "[StorageV2] translator {} cell {} not loaded", key_, cid));
        cells.emplace_back(cid, std::move(it->second));
    }

    return cells;
}

std::unique_ptr<milvus::GroupChunk>
ManifestGroupTranslator::load_group_chunk(
    const std::vector<std::shared_ptr<arrow::Table>>& tables,
    const milvus::cachinglayer::cid_t cid,
    std::optional<FieldId> only_field) {
    assert(!tables.empty());
    // Use the first table's schema as reference for field iteration
    const auto& schema = tables[0]->schema();

    std::vector<FieldId> field_ids;
    field_ids.reserve(schema->num_fields());
    std::vector<FieldMeta> field_metas;
    field_metas.reserve(schema->num_fields());
    std::vector<arrow::ArrayVector> array_vecs;
    array_vecs.reserve(schema->num_fields());

    // Iterate through fields to get field_id and create chunk.
    // Normal collections and Milvus-generated columns store field IDs as
    // column names. Other external columns use external_field names.
    for (int i = 0; i < schema->num_fields(); ++i) {
        auto column_name = schema->field(i)->name();
        int64_t field_id = -1;
        if (auto parsed_fid = ParseFieldIdColumnName(column_name);
            parsed_fid.has_value()) {
            field_id = parsed_fid->get();
        } else {
            // External collection fallback: column_name is non-numeric, so it
            // comes from an external manifest external_field mapping. Normal
            // fields and function-output fields are stored by numeric field id
            // and take the strict field-id path above.
            for (const auto& [fid, meta] : field_metas_) {
                if (meta.is_external_field() &&
                    meta.get_external_field() == column_name) {
                    field_id = fid.get();
                    break;
                }
            }
        }
        if (field_id < 0) {
            AssertInfo(
                false,
                "[StorageV2] translator {} field {} not a numeric field ID "
                "and not found as external field",
                key_,
                column_name);
        }

        auto fid = milvus::FieldId(field_id);
        if (fid == RowFieldID) {
            // ignore row id field
            continue;
        }
        // Coalesced split: materialize only the requested field, leaving the
        // other columns in `tables` for their own per-field cells.
        if (only_field.has_value() && fid != *only_field) {
            continue;
        }
        auto it = field_metas_.find(fid);
        AssertInfo(
            it != field_metas_.end(),
            "[StorageV2] translator {} field id {} not found in field_metas",
            key_,
            fid.get());
        const auto& field_meta = it->second;

        // Merge arrays from all tables for this field
        // All tables in a cell come from the same column group with consistent schema
        arrow::ArrayVector merged_array_vec;
        for (const auto& table : tables) {
            auto chunks = table->column(i)->chunks();
            merged_array_vec.insert(
                merged_array_vec.end(), chunks.begin(), chunks.end());
        }

        field_ids.push_back(fid);
        field_metas.push_back(field_meta);
        array_vecs.push_back(std::move(merged_array_vec));
    }

    // Normalize all arrow arrays for ChunkWriter compatibility.
    // Handles: vectors (nullable/non-nullable), strings, timestamps,
    // arrays, vector arrays, JSON, geometry.
    for (size_t idx = 0; idx < field_ids.size(); ++idx) {
        array_vecs[idx] = storage::NormalizeArrowForChunkWriter(
            array_vecs[idx], field_metas[idx]);
    }

    std::unordered_map<FieldId, std::shared_ptr<Chunk>> chunks;
    if (!use_mmap_) {
        // Memory mode
        chunks = create_group_chunk(
            field_ids, field_metas, array_vecs, mmap_populate_);
    } else {
        // Mmap mode — use unique generation suffix to avoid truncating files
        // that old MAP_SHARED mmaps still reference (see #48658).
        auto gen =
            g_mmap_path_generation.fetch_add(1, std::memory_order_relaxed);
        std::filesystem::path filepath;
        switch (group_chunk_type_) {
            case GroupChunkType::DEFAULT:
                filepath = std::filesystem::path(mmap_dir_path_) /
                           fmt::format("seg_{}_cg_{}_{}_{}",
                                       segment_id_,
                                       column_group_index_,
                                       cid,
                                       gen);
                break;
            case GroupChunkType::JSON_KEY_STATS:
                filepath =
                    std::filesystem::path(mmap_dir_path_) /
                    fmt::format(
                        "seg_{}_jks_{}_cg_{}_{}_{}",
                        segment_id_,
                        // NOTE: here we assume the first field is the main field for json key stats group chunk
                        std::to_string(field_metas[0].get_main_field_id()),
                        column_group_index_,
                        cid,
                        gen);
                break;
            default:
                ThrowInfo(ErrorCode::UnexpectedError,
                          "unknown group chunk type: {}",
                          static_cast<uint8_t>(group_chunk_type_));
        }
        std::filesystem::create_directories(filepath.parent_path());
        chunks = create_group_chunk(field_ids,
                                    field_metas,
                                    array_vecs,
                                    mmap_populate_,
                                    filepath.string(),
                                    load_priority_);
    }

    return std::make_unique<milvus::GroupChunk>(chunks);
}

int64_t
ManifestGroupTranslator::loading_overhead_bytes(size_t field_index,
                                                int64_t cell_size) const {
    bool is_array = field_index < meta_.field_is_array_.size() &&
                    meta_.field_is_array_[field_index] != 0;
    if (!is_array) {
        return cell_size;
    }
    if (cell_size > std::numeric_limits<int64_t>::max() / 2) {
        return std::numeric_limits<int64_t>::max();
    }
    return cell_size * 2;
}

}  // namespace milvus::segcore::storagev2translator
