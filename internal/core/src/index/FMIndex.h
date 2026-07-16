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

#pragma once

#include <sys/mman.h>
#include <unistd.h>

#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include "common/EasyAssert.h"
#include "common/Types.h"
#include "index/IndexInfo.h"
#include "index/Meta.h"
#include "index/ScalarIndex.h"
#include "index/fmindex/FMIndex.h"
#include "pb/schema.pb.h"
#include "storage/DiskFileManagerImpl.h"
#include "storage/FileManager.h"
#include "storage/MemFileManagerImpl.h"

namespace milvus::index {

// File-name / meta keys used by the V3 packed-file entries. These strings are
// persisted, do not edit them.
constexpr const char* FMINDEX_BLOB_FILE_NAME = "fm_index.bin";
constexpr const char* FMINDEX_ROW_LEN_FILE_NAME = "fm_index_row_len";
constexpr const char* FMINDEX_META_TOTAL_ROWS = "total_rows";
constexpr const char* FMINDEX_META_NULLABLE = "nullable";

// Scalar-index wrapper around the self-contained byte-exact FM-index library
// (milvus::index::fmindex::FMIndex). It accelerates LIKE prefix/infix/suffix on
// VARCHAR exactly (no recheck). General LIKE / regex / range fall back to the
// raw-data scan (see ShouldUseOp).
class FMIndex : public ScalarIndex<std::string> {
 public:
    using MemFileManager = storage::MemFileManagerImpl;
    using MemFileManagerPtr = std::shared_ptr<MemFileManager>;

    explicit FMIndex(const storage::FileManagerContext& ctx,
                     const FMIndexParams& params);

    ~FMIndex() {
        // fm_ may view the mmap'd region (LoadView path); unmap after the
        // wrapper body runs and before fm_'s own (view-only) destruction.
        if (is_mmap_ && mmap_data_ != nullptr && mmap_data_ != MAP_FAILED) {
            munmap(mmap_data_, mmap_size_);
            unlink(mmap_filepath_.c_str());
        }
    }

    ScalarIndexType
    GetIndexType() const override {
        return ScalarIndexType::FMINDEX;
    }

    // ---- deprecated / unsupported entry points ----

    void
    Load(const BinarySet& binary_set, const Config& config = {}) override {
        ThrowInfo(ErrorCode::NotImplemented, "load v1 deprecated");
    }

    void
    Load(milvus::tracer::TraceContext ctx, const Config& config = {}) override;

    void
    BuildWithDataset(const DatasetPtr& dataset,
                     const Config& config = {}) override {
        ThrowInfo(ErrorCode::NotImplemented,
                  "BuildWithDataset should be deprecated");
    }

    // deprecated, only used in small chunk index.
    void
    Build(size_t n,
          const std::string* values,
          const bool* valid_data) override {
        ThrowInfo(ErrorCode::NotImplemented, "Build should not be called");
    }

    // ---- build ----

    void
    Build(const Config& config = {}) override;

    void
    BuildWithFieldData(const std::vector<FieldDataPtr>& datas) override;

    // BuildWithRawDataForUT should be only used in ut. Only string is supported.
    void
    BuildWithRawDataForUT(size_t n,
                          const void* values,
                          const Config& config = {}) override;

    // ---- serialize / upload / load (V3 packed-file path) ----

    BinarySet
    Serialize(const Config& config) override;

    IndexStatsPtr
    Upload(const Config& config = {}) override;

    void
    WriteEntries(storage::IndexEntryWriter* writer) override;

    void
    LoadEntries(storage::IndexEntryReader& reader,
                const Config& config) override;

    // ---- query ----

    const TargetBitmap
    In(size_t n, const std::string* values) override;

    const TargetBitmap
    NotIn(size_t n, const std::string* values) override;

    const TargetBitmap
    IsNull() override;

    TargetBitmap
    IsNotNull() override;

    const TargetBitmap
    Range(const std::string& value, OpType op) override {
        ThrowInfo(ErrorCode::Unsupported, "FM-index does not support range");
    }

    const TargetBitmap
    Range(const std::string& lower_bound_value,
          bool lb_inclusive,
          const std::string& upper_bound_value,
          bool ub_inclusive) override {
        ThrowInfo(ErrorCode::Unsupported, "FM-index does not support range");
    }

    const TargetBitmap
    PatternMatch(const std::string& pattern, proto::plan::OpType op) override;

    bool
    SupportPatternMatch() const override {
        return true;
    }

    // Only the exact anchored/substring pattern ops can be answered exactly
    // (no recheck) by the FM-index. General LIKE / regex / range fall back to
    // the raw-data scan.
    bool
    ShouldUseOp(proto::plan::OpType op) const override {
        switch (op) {
            case proto::plan::OpType::PrefixMatch:
            case proto::plan::OpType::PostfixMatch:
            case proto::plan::OpType::InnerMatch:
                return true;
            // General LIKE / regex are not answered exactly in v1; decline so
            // the executor runs the brute-force scan path.
            case proto::plan::OpType::Match:
            case proto::plan::OpType::RegexMatch:
            // Lexicographic range needs forward navigation the FM-index does
            // not carry (Range() throws Unsupported). Decline so the executor
            // downgrades to the raw-data scan (UnaryIndexFunc otherwise calls
            // index->Range() directly for these ops). Without this, the base
            // ShouldUseOp default returns true for range and the query throws
            // instead of scanning.
            case proto::plan::OpType::GreaterThan:
            case proto::plan::OpType::GreaterEqual:
            case proto::plan::OpType::LessThan:
            case proto::plan::OpType::LessEqual:
                return false;
            default:
                return true;
        }
    }

    // ---- misc ----

    int64_t
    Count() override {
        return total_rows_;
    }

    int64_t
    Size() override {
        return Count();
    }

    const bool
    HasRawData() const override {
        return false;
    }

    std::optional<std::string>
    Reverse_Lookup(size_t offset) const override {
        ThrowInfo(ErrorCode::NotImplemented,
                  "Reverse_Lookup should not be handled by FM index");
    }

 private:
    // Turn a sorted, unique list of matching doc ids into a bitmap over rows.
    TargetBitmap
    DocsToBitmap(const std::vector<uint64_t>& docs) const;

    fmindex::FMIndex fm_;

    proto::schema::FieldSchema schema_;
    int64_t field_id_{0};

    // For the mmap load path: streams the FM-index blob to a local file and
    // maps it, so fm_ (via LoadView) serves queries zero-copy from the mapping.
    std::shared_ptr<storage::DiskFileManagerImpl> disk_file_manager_;
    bool is_mmap_{false};
    char* mmap_data_{nullptr};
    size_t mmap_size_{0};
    std::string mmap_filepath_;

    uint32_t sa_sample_rate_{8};
    bool loading_index_{false};

    // number of rows indexed (one document per row, including nulls)
    int64_t total_rows_{0};

    // byte length of each row's value (0 for null). Used by In/NotIn to turn a
    // prefix hit into an exact whole-string equality test.
    std::vector<uint32_t> row_len_;

    // bit i set iff row i is null. Rebuilt on build and on load.
    TargetBitmap null_bitmap_;
};

}  // namespace milvus::index
