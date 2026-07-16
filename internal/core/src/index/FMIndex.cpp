// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License

#include "index/FMIndex.h"

#include <fcntl.h>

#include <cerrno>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

#include "common/EasyAssert.h"
#include "common/FastMem.h"
#include "common/FieldDataInterface.h"
#include "common/File.h"
#include "common/Slice.h"
#include "common/Tracer.h"
#include "index/Meta.h"
#include "index/Utils.h"
#include "knowhere/binaryset.h"
#include "log/Log.h"
#include "storage/DiskFileManagerImpl.h"
#include "storage/FileWriter.h"
#include "storage/IndexEntryReader.h"
#include "storage/IndexEntryWriter.h"
#include "storage/MemFileManagerImpl.h"
#include "storage/Util.h"

namespace milvus::index {

namespace {

// bytes(s): view a string's data as raw bytes for the FM-index library.
inline const uint8_t*
bytes(const std::string& s) {
    return reinterpret_cast<const uint8_t*>(s.data());
}

// Trailing slack appended to the mmap'd blob file so any word-granular read at
// the very end of the FM-index stays inside the mapping.
constexpr size_t kFMIndexMmapPadding = 64;

// Sentinel stored in row_len_ for a null row. Milvus VARCHAR values are capped
// far below 2^32 bytes, so this can never collide with a real length — it lets
// LoadEntries distinguish a null row from a genuine empty string (length 0),
// which a plain "len == 0 means null" rule would conflate.
constexpr uint32_t kFMIndexNullRowLen = UINT32_MAX;

}  // namespace

FMIndex::FMIndex(const storage::FileManagerContext& ctx,
                 const FMIndexParams& params)
    : ScalarIndex<std::string>(FMINDEX_INDEX_TYPE) {
    schema_ = ctx.fieldDataMeta.field_schema;
    field_id_ = ctx.fieldDataMeta.field_id;
    sa_sample_rate_ = params.sa_sample_rate == 0 ? 8 : params.sa_sample_rate;
    loading_index_ = params.loading_index;
    // Invalid context = unit test (no remote storage): skip file managers.
    if (ctx.Valid()) {
        this->file_manager_ = std::make_shared<MemFileManager>(ctx);
        disk_file_manager_ =
            std::make_shared<storage::DiskFileManagerImpl>(ctx);
    }
}

void
FMIndex::Build(const Config& config) {
    auto field_datas = storage::CacheRawDataAndFillMissing(
        std::static_pointer_cast<MemFileManager>(this->file_manager_), config);
    BuildWithFieldData(field_datas);
}

void
FMIndex::BuildWithFieldData(const std::vector<FieldDataPtr>& datas) {
    AssertInfo(schema_.data_type() == proto::schema::DataType::String ||
                   schema_.data_type() == proto::schema::DataType::VarChar,
               "FM index only supports String/VarChar, got data type {}",
               schema_.data_type());
    LOG_INFO("Start to build FM index, field id: {}", field_id_);

    std::vector<std::string_view> docs;
    std::vector<bool> valid;
    for (const auto& data : datas) {
        auto n = data->get_num_rows();
        for (int64_t i = 0; i < n; i++) {
            if (schema_.nullable() && !data->is_valid(i)) {
                docs.emplace_back();
                valid.push_back(false);
                row_len_.push_back(kFMIndexNullRowLen);
            } else {
                auto* s = static_cast<const std::string*>(data->RawValue(i));
                docs.emplace_back(*s);
                valid.push_back(true);
                row_len_.push_back(static_cast<uint32_t>(s->size()));
            }
        }
    }

    total_rows_ = static_cast<int64_t>(docs.size());

    // Build the null bitmap (bit i set iff row i is null).
    null_bitmap_ = TargetBitmap(total_rows_);
    for (int64_t i = 0; i < total_rows_; i++) {
        if (!valid[i]) {
            null_bitmap_.set(i);
        }
    }

    // The string_views point into the FieldData which stays alive through the
    // call; the library copies internally during Build.
    fm_.Build(docs, sa_sample_rate_);

    LOG_INFO("FM index build done, field id: {}, total rows: {}",
             field_id_,
             total_rows_);
}

void
FMIndex::BuildWithRawDataForUT(size_t n,
                               const void* values,
                               const Config& config) {
    schema_.set_data_type(proto::schema::DataType::VarChar);

    auto* strs = static_cast<const std::string*>(values);
    std::vector<std::string_view> docs;
    docs.reserve(n);
    row_len_.reserve(n);
    for (size_t i = 0; i < n; i++) {
        docs.emplace_back(strs[i]);
        row_len_.push_back(static_cast<uint32_t>(strs[i].size()));
    }

    total_rows_ = static_cast<int64_t>(n);
    null_bitmap_ = TargetBitmap(total_rows_);

    fm_.Build(docs, sa_sample_rate_);
}

TargetBitmap
FMIndex::DocsToBitmap(const std::vector<uint64_t>& docs) const {
    TargetBitmap bitset(total_rows_);
    for (auto d : docs) {
        bitset.set(d);
    }
    return bitset;
}

const TargetBitmap
FMIndex::PatternMatch(const std::string& pattern, proto::plan::OpType op) {
    tracer::AutoSpan span("FMIndex::PatternMatch", tracer::GetRootSpan());
    // Empty pattern: the planner lowers a match-anything LIKE (`LIKE '%'`,
    // `LIKE '%%'`, ...) to an anchored op with an empty literal — PrefixMatch(""),
    // InnerMatch("") or PostfixMatch(""). Every string trivially has "" as a
    // prefix, substring and suffix, so the correct result is ALL non-null rows.
    // The FM-index library short-circuits an empty needle to ZERO hits, so
    // falling through to Locate*/Matching* below would wrongly return the empty
    // set; answer it directly with IsNotNull(). Null rows are excluded, matching
    // SQL `NULL LIKE '%'` = NULL (not true).
    if (pattern.empty()) {
        switch (op) {
            case proto::plan::OpType::PrefixMatch:
            case proto::plan::OpType::PostfixMatch:
            case proto::plan::OpType::InnerMatch:
                return IsNotNull();
            default:
                break;  // fall through so the op switch throws OpTypeInvalid.
        }
    }
    // The executor passes the RAW literal (PrefixMatch gets "abc" not "abc%"),
    // so pass the pattern verbatim. These results are EXACT (no recheck).
    switch (op) {
        case proto::plan::OpType::PrefixMatch:
            return DocsToBitmap(
                fm_.LocatePrefixDocs(bytes(pattern), pattern.size()));
        case proto::plan::OpType::PostfixMatch:
            return DocsToBitmap(
                fm_.LocateSuffixDocs(bytes(pattern), pattern.size()));
        case proto::plan::OpType::InnerMatch:
            return DocsToBitmap(
                fm_.MatchingDocs(bytes(pattern), pattern.size()));
        default:
            ThrowInfo(ErrorCode::OpTypeInvalid,
                      "not supported op type: {} for FM index PatternMatch",
                      op);
    }
}

const TargetBitmap
FMIndex::In(size_t n, const std::string* values) {
    tracer::AutoSpan span("FMIndex::In", tracer::GetRootSpan());
    TargetBitmap bitset(total_rows_);
    // Exact whole-string equality via prefix-anchored + length: a doc EQUALS v
    // iff it begins with v AND its length == v.length().
    for (size_t i = 0; i < n; i++) {
        const auto& v = values[i];
        if (v.empty()) {
            // The library short-circuits an empty pattern to zero hits, so the
            // prefix walk below can never match the empty string. Handle it
            // directly: a non-null row equals "" iff its length is 0.
            for (int64_t d = 0; d < total_rows_; d++) {
                if (row_len_[d] == 0 && !null_bitmap_[d]) {
                    bitset.set(d);
                }
            }
            continue;
        }
        auto docs = fm_.LocatePrefixDocs(bytes(v), v.size());
        for (auto d : docs) {
            if (row_len_[d] == v.size()) {
                bitset.set(d);
            }
        }
    }
    return bitset;
}

const TargetBitmap
FMIndex::NotIn(size_t n, const std::string* values) {
    tracer::AutoSpan span("FMIndex::NotIn", tracer::GetRootSpan());
    // Compute the exact equality-set (same logic as In), flip over all rows,
    // then clear the null rows so that null rows are never returned by NotIn
    // (complement over non-null rows).
    TargetBitmap bitset(total_rows_);
    for (size_t i = 0; i < n; i++) {
        const auto& v = values[i];
        if (v.empty()) {
            // Empty pattern short-circuits to zero hits in the library; a
            // non-null row equals "" iff its length is 0. See In() for detail.
            for (int64_t d = 0; d < total_rows_; d++) {
                if (row_len_[d] == 0 && !null_bitmap_[d]) {
                    bitset.set(d);
                }
            }
            continue;
        }
        auto docs = fm_.LocatePrefixDocs(bytes(v), v.size());
        for (auto d : docs) {
            if (row_len_[d] == v.size()) {
                bitset.set(d);
            }
        }
    }
    bitset.flip();
    for (int64_t i = 0; i < total_rows_; i++) {
        if (null_bitmap_[i]) {
            bitset.reset(i);
        }
    }
    return bitset;
}

const TargetBitmap
FMIndex::IsNull() {
    tracer::AutoSpan span("FMIndex::IsNull", tracer::GetRootSpan());
    return null_bitmap_.clone();
}

TargetBitmap
FMIndex::IsNotNull() {
    tracer::AutoSpan span("FMIndex::IsNotNull", tracer::GetRootSpan());
    TargetBitmap bitset = null_bitmap_.clone();
    bitset.flip();
    return bitset;
}

BinarySet
FMIndex::Serialize(const Config& config) {
    // The FM-index persists through the V3 packed-file path (WriteEntries).
    // V1 BinarySet serialization is not used; return an empty set.
    BinarySet res_set;
    milvus::Disassemble(res_set);
    return res_set;
}

IndexStatsPtr
FMIndex::Upload(const Config& config) {
    LOG_INFO("Start to upload FM index, field id: {}, total rows: {}",
             field_id_,
             total_rows_);
    return UploadUnified(config);
}

void
FMIndex::Load(milvus::tracer::TraceContext ctx, const Config& config) {
    LoadUnified(config);
}

void
FMIndex::WriteEntries(storage::IndexEntryWriter* writer) {
    // One flat blob for the whole FM-index.
    std::string blob = fm_.Serialize();
    writer->WriteEntry(FMINDEX_BLOB_FILE_NAME, blob.data(), blob.size());

    // Per-row byte lengths, used by In/NotIn for exact equality.
    writer->WriteEntry(FMINDEX_ROW_LEN_FILE_NAME,
                       row_len_.data(),
                       row_len_.size() * sizeof(uint32_t));

    writer->PutMeta(FMINDEX_META_TOTAL_ROWS, total_rows_);
    writer->PutMeta(FMINDEX_META_NULLABLE, schema_.nullable());

    LOG_INFO(
        "WriteEntries FM index done, field id: {}, total rows: {}, blob size: "
        "{}",
        field_id_,
        total_rows_,
        blob.size());
}

void
FMIndex::LoadEntries(storage::IndexEntryReader& reader, const Config& config) {
    total_rows_ = reader.GetMeta<int64_t>(FMINDEX_META_TOTAL_ROWS);
    bool nullable = reader.GetMeta<bool>(FMINDEX_META_NULLABLE);

    // Load the FM-index blob. With mmap enabled (default), stream it to a local
    // file, map it, and view it in place (LoadView, zero-copy) — the FM-index
    // serves queries straight out of the mapping. Otherwise copy the bytes into
    // an owned buffer (Deserialize).
    is_mmap_ = GetValueFromConfig<bool>(config, ENABLE_MMAP).value_or(true);
    auto load_priority =
        GetValueFromConfig<milvus::proto::common::LoadPriority>(
            config, milvus::LOAD_PRIORITY)
            .value_or(milvus::proto::common::LoadPriority::HIGH);

    if (is_mmap_ && disk_file_manager_ != nullptr) {
        mmap_filepath_ = disk_file_manager_->GetLocalIndexObjectPrefix() +
                         std::string("/") + FMINDEX_BLOB_FILE_NAME;
        std::filesystem::create_directories(
            std::filesystem::path(mmap_filepath_).parent_path());
        size_t blob_size = 0;
        {
            auto file_writer = storage::FileWriter(
                mmap_filepath_,
                storage::io::GetPriorityFromLoadPriority(load_priority));
            reader.ReadEntryStream(FMINDEX_BLOB_FILE_NAME,
                                   [&](const uint8_t* data, size_t len) {
                                       file_writer.Write(data, len);
                                       blob_size += len;
                                   });
            std::vector<uint8_t> pad(kFMIndexMmapPadding, 0);
            file_writer.Write(pad.data(), pad.size());
            file_writer.Finish();
        }
        mmap_size_ = blob_size + kFMIndexMmapPadding;
        auto file = File::Open(mmap_filepath_, O_RDONLY);
        mmap_data_ = static_cast<char*>(mmap(
            nullptr, mmap_size_, PROT_READ, MAP_PRIVATE, file.Descriptor(), 0));
        file.Close();
        AssertInfo(mmap_data_ != MAP_FAILED,
                   "failed to mmap FM index: {}",
                   strerror(errno));
        // The move keeps the views pointing at mmap_data_ (the vector buffers
        // move by address); the mapping is unmapped in the destructor.
        fm_ = fmindex::FMIndex::LoadView(
            reinterpret_cast<const uint8_t*>(mmap_data_), blob_size);
    } else {
        auto blob_entry = reader.ReadEntry(FMINDEX_BLOB_FILE_NAME);
        fm_ = fmindex::FMIndex::Deserialize(
            std::string(reinterpret_cast<const char*>(blob_entry.data.data()),
                        blob_entry.data.size()));
    }
    AssertInfo(fm_.valid(), "failed to load FM index blob");

    // Load per-row byte lengths.
    auto row_len_entry = reader.ReadEntry(FMINDEX_ROW_LEN_FILE_NAME);
    row_len_.resize(row_len_entry.data.size() / sizeof(uint32_t));
    milvus::fastmem::FastMemcpy(
        row_len_.data(), row_len_entry.data.data(), row_len_entry.data.size());

    // Rebuild the null bitmap from row_len_: a row is null iff it carries the
    // null sentinel. This keeps a genuine empty string (length 0) distinct from
    // a null row, matching the build-time convention. `nullable` is informational
    // only — a non-nullable field simply never stores the sentinel.
    (void)nullable;
    null_bitmap_ = TargetBitmap(total_rows_);
    for (int64_t i = 0; i < total_rows_; i++) {
        if (row_len_[i] == kFMIndexNullRowLen) {
            null_bitmap_.set(i);
        }
    }

    LOG_INFO("LoadEntries FM index done, field id: {}, total rows: {}",
             field_id_,
             total_rows_);
}

}  // namespace milvus::index
