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

#include <fmt/core.h>
#include <gtest/gtest.h>

#include <memory>
#include <string>
#include <vector>

#include "common/Consts.h"
#include "common/FieldData.h"
#include "common/Schema.h"
#include "common/Types.h"
#include "index/FMIndex.h"
#include "index/IndexInfo.h"
#include "index/Meta.h"
#include "pb/common.pb.h"
#include "pb/plan.pb.h"
#include "storage/FileManager.h"
#include "storage/InsertData.h"
#include "storage/PayloadReader.h"
#include "storage/RemoteChunkManagerSingleton.h"
#include "storage/Types.h"
#include "storage/Util.h"
#include "test_utils/Constants.h"
#include "test_utils/DataGen.h"
#include "test_utils/storage_test_utils.h"

using namespace milvus;

namespace {

// Collect the set-bit positions of a bitmap into a sorted vector, for readable
// equality assertions against an expected id list.
std::vector<int64_t>
SetBits(const TargetBitmap& bitmap) {
    std::vector<int64_t> out;
    for (size_t i = 0; i < bitmap.size(); i++) {
        if (bitmap[i]) {
            out.push_back(static_cast<int64_t>(i));
        }
    }
    return out;
}

// Build an in-memory FM index over `data` via the unit-test path (no storage),
// which is enough to exercise the query routing (prefix/infix/suffix, In/NotIn).
std::unique_ptr<index::FMIndex>
MakeRawDataIndex(const std::vector<std::string>& data) {
    index::FMIndexParams params{/*loading_index=*/false, /*sa_sample_rate=*/32};
    auto idx =
        std::make_unique<index::FMIndex>(storage::FileManagerContext(), params);
    idx->BuildWithRawDataForUT(data.size(), data.data());
    return idx;
}

// TargetBitmap's copy ctor is deleted, so these return the set-bit id list
// directly (binding the returned bitmap by const ref, never copying it).
std::vector<int64_t>
Prefix(index::FMIndex* idx, const std::string& p) {
    const auto& bm = idx->PatternMatch(p, proto::plan::OpType::PrefixMatch);
    return SetBits(bm);
}
std::vector<int64_t>
Suffix(index::FMIndex* idx, const std::string& p) {
    const auto& bm = idx->PatternMatch(p, proto::plan::OpType::PostfixMatch);
    return SetBits(bm);
}
std::vector<int64_t>
Inner(index::FMIndex* idx, const std::string& p) {
    const auto& bm = idx->PatternMatch(p, proto::plan::OpType::InnerMatch);
    return SetBits(bm);
}

}  // namespace

// ---- query routing over raw data (no storage) ----

TEST(FMIndex, PrefixInfixSuffixRouting) {
    // idx:   0        1        2         3        4              5      6
    std::vector<std::string> data{
        "apple", "apply", "banana", "grape", "application", "app", ""};
    auto idx = MakeRawDataIndex(data);

    EXPECT_EQ(idx->Count(), 7);

    // prefix "app": apple, apply, application, app (not "" / banana / grape)
    EXPECT_EQ(Prefix(idx.get(), "app"), (std::vector<int64_t>{0, 1, 4, 5}));
    // suffix "e": apple, grape
    EXPECT_EQ(Suffix(idx.get(), "e"), (std::vector<int64_t>{0, 3}));
    // infix "pp": apple, apply, application, app
    EXPECT_EQ(Inner(idx.get(), "pp"), (std::vector<int64_t>{0, 1, 4, 5}));
    // infix "an": only banana
    EXPECT_EQ(Inner(idx.get(), "an"), (std::vector<int64_t>{2}));
    // infix that matches nothing
    EXPECT_TRUE(Inner(idx.get(), "xyz").empty());
}

// `LIKE '%'` lowers to an anchored op with an EMPTY literal (PrefixMatch("") /
// InnerMatch("") / PostfixMatch("")). Every string has "" as a prefix, substring
// and suffix, so all (non-null) rows must match — the FM library short-circuits
// an empty needle to zero hits, and PatternMatch must not let that leak through
// as an empty result.
TEST(FMIndex, EmptyPatternMatchesAllRows) {
    std::vector<std::string> data{
        "apple", "apply", "banana", "grape", "application", "app", ""};
    auto idx = MakeRawDataIndex(data);

    std::vector<int64_t> all{0, 1, 2, 3, 4, 5, 6};
    EXPECT_EQ(Prefix(idx.get(), ""), all);
    EXPECT_EQ(Suffix(idx.get(), ""), all);
    EXPECT_EQ(Inner(idx.get(), ""), all);
}

// ShouldUseOp is the executor's routing gate (UnaryIndexFunc): true routes the
// op to this index, false downgrades to the raw-data scan. Range ops MUST be
// declined — Range() throws Unsupported, so routing them here would fail the
// query (`field > "x"`, BETWEEN) instead of scanning. Match/RegexMatch are not
// answered exactly in v1 and must also decline.
TEST(FMIndex, ShouldUseOpDeclinesRangeAndRegex) {
    auto idx = MakeRawDataIndex({"apple", "banana"});

    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PrefixMatch));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PostfixMatch));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch));

    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::GreaterThan));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::GreaterEqual));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::LessThan));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::LessEqual));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::Match));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::RegexMatch));

    // And the ops it declines to route would indeed throw if reached directly.
    EXPECT_THROW(idx->Range("m", OpType::GreaterThan), SegcoreError);
}

// InnerMatch over rows that each contain the pattern MANY times: the result is
// per-row (each matching row set once), and the streaming visitor path must
// dedup repeated in-row occurrences without materializing them.
TEST(FMIndex, InnerMatchDedupsRepeatedOccurrences) {
    // row 0: "ababababab..." (50 occurrences of "ab")
    // row 1: no match; row 2: one occurrence; row 3: "ab" x 200
    std::string many0, many3;
    for (int i = 0; i < 50; i++) {
        many0 += "ab";
    }
    for (int i = 0; i < 200; i++) {
        many3 += "ab";
    }
    auto idx = MakeRawDataIndex({many0, "zzzz", "xxabxx", many3});

    EXPECT_EQ(Inner(idx.get(), "ab"), (std::vector<int64_t>{0, 2, 3}));
    // single-char patterns, the degenerate high-frequency case
    EXPECT_EQ(Inner(idx.get(), "a"), (std::vector<int64_t>{0, 2, 3}));
    EXPECT_EQ(Inner(idx.get(), "z"), (std::vector<int64_t>{1}));
    EXPECT_EQ(Inner(idx.get(), "x"), (std::vector<int64_t>{2}));
}

TEST(FMIndex, InNotInExactEquality) {
    std::vector<std::string> data{
        "apple", "apply", "banana", "grape", "application", "app", ""};
    auto idx = MakeRawDataIndex(data);

    // "app" as a prefix hits 4 docs, but only doc 5 EQUALS "app".
    std::string app = "app";
    EXPECT_EQ(SetBits(idx->In(1, &app)), (std::vector<int64_t>{5}));

    // exact whole-string
    std::string banana = "banana";
    EXPECT_EQ(SetBits(idx->In(1, &banana)), (std::vector<int64_t>{2}));

    // empty string equals only the empty doc (doc 6)
    std::string empty = "";
    EXPECT_EQ(SetBits(idx->In(1, &empty)), (std::vector<int64_t>{6}));

    // NotIn("app") is the complement over all rows (no nulls present)
    EXPECT_EQ(SetBits(idx->NotIn(1, &app)),
              (std::vector<int64_t>{0, 1, 2, 3, 4, 6}));

    // In with multiple values is the union
    std::vector<std::string> vs{"apple", "grape"};
    EXPECT_EQ(SetBits(idx->In(2, vs.data())), (std::vector<int64_t>{0, 3}));
}

// ---- full storage round-trip (Build -> Upload -> Load), mmap on and off ----

namespace {

void
CheckLoadedQueries(index::FMIndex* idx) {
    EXPECT_EQ(Prefix(idx, "app"), (std::vector<int64_t>{0, 1, 4, 5}));
    EXPECT_EQ(Suffix(idx, "e"), (std::vector<int64_t>{0, 3}));
    EXPECT_EQ(Inner(idx, "pp"), (std::vector<int64_t>{0, 1, 4, 5}));
    std::string app = "app";
    EXPECT_EQ(SetBits(idx->In(1, &app)), (std::vector<int64_t>{5}));
    std::string empty = "";
    EXPECT_EQ(SetBits(idx->In(1, &empty)), (std::vector<int64_t>{6}));
}

// Build -> Upload -> Load through one shared cm/fs/ctx (the test chunk manager
// keeps index files under an instance-local root, so a load routed through a
// second manager instance would not find them). Exercises WriteEntries and
// LoadEntries for both the mmap (LoadView) and the copy (Deserialize) paths.
void
RunRoundTrip(bool enable_mmap) {
    int64_t collection_id = 1, partition_id = 2, segment_id = 3;
    int64_t index_build_id = 4000, index_version = 4000;

    auto schema = std::make_shared<Schema>();
    auto field_id = schema->AddDebugField("fm", DataType::VARCHAR, false);
    auto field_meta = milvus::segcore::gen_field_meta(collection_id,
                                                      partition_id,
                                                      segment_id,
                                                      field_id.get(),
                                                      DataType::VARCHAR,
                                                      DataType::NONE,
                                                      false);
    auto index_meta = gen_index_meta(
        segment_id, field_id.get(), index_build_id, index_version);

    auto storage_config = gen_local_storage_config(TestLocalPath);
    auto cm = CreateChunkManager(storage_config);
    auto fs = storage::InitArrowFileSystem(storage_config);
    storage::FileManagerContext ctx(field_meta, index_meta, cm, fs);

    std::vector<std::string> data{
        "apple", "apply", "banana", "grape", "application", "app", ""};
    auto field_data =
        storage::CreateFieldData(DataType::VARCHAR, DataType::NONE, false);
    field_data->FillFieldData(data.data(), data.size());

    auto payload_reader =
        std::make_shared<milvus::storage::PayloadReader>(field_data);
    storage::InsertData insert_data(payload_reader);
    insert_data.SetFieldDataMeta(field_meta);
    insert_data.SetTimestamps(0, 100);
    auto serialized_bytes = insert_data.Serialize(storage::Remote);

    auto log_path = fmt::format("{}{}/{}/{}/{}/{}",
                                TestLocalPath,
                                collection_id,
                                partition_id,
                                segment_id,
                                field_id.get(),
                                0);
    auto cm_w = ChunkManagerWrapper(cm);
    cm_w.Write(log_path, serialized_bytes.data(), serialized_bytes.size());

    std::vector<std::string> index_files;
    {
        Config config;
        config[milvus::index::INDEX_TYPE] = milvus::index::FMINDEX_INDEX_TYPE;
        config[INSERT_FILES_KEY] = std::vector<std::string>{log_path};
        index::FMIndexParams params{false, 32};
        auto index = std::make_shared<index::FMIndex>(ctx, params);
        index->Build(config);
        index_files = index->UploadUnified({})->GetIndexFiles();
    }

    Config config;
    config[milvus::index::INDEX_FILES] = index_files;
    config[milvus::LOAD_PRIORITY] = milvus::proto::common::LoadPriority::HIGH;
    config[milvus::index::ENABLE_MMAP] = enable_mmap;
    index::FMIndexParams params{true, 32};
    auto idx = std::make_unique<index::FMIndex>(ctx, params);
    idx->LoadUnified(config);

    EXPECT_EQ(idx->Count(), 7);
    CheckLoadedQueries(idx.get());
}

}  // namespace

TEST(FMIndex, SerializeLoadRoundTripMmap) {
    RunRoundTrip(/*enable_mmap=*/true);
}

TEST(FMIndex, SerializeLoadRoundTripNoMmap) {
    RunRoundTrip(/*enable_mmap=*/false);
}

// A genuine empty string must stay distinct from null across a serialize/load
// cycle: In("") matches the empty row but not the null row, and IsNull marks
// only the null row.
TEST(FMIndex, NullVsEmptyStringDistinctAfterReload) {
    // idx:   0        1     2(null)  3
    std::vector<std::string> values{"apple", "", "", "banana"};
    std::vector<bool> valid{true, true, false, true};
    // Pack validity into a bitmap (bit i set == row i is valid), as the nullable
    // FillFieldData overload expects.
    std::vector<uint8_t> valid_bitmap((values.size() + 7) / 8, 0);
    for (size_t i = 0; i < valid.size(); i++) {
        if (valid[i]) {
            valid_bitmap[i >> 3] |= (1u << (i & 0x07));
        }
    }

    auto schema = std::make_shared<Schema>();
    auto field_id = schema->AddDebugField("fm", DataType::VARCHAR, true);
    int64_t collection_id = 1, partition_id = 2, segment_id = 3;
    int64_t index_build_id = 4001, index_version = 4001;

    auto field_meta = milvus::segcore::gen_field_meta(collection_id,
                                                      partition_id,
                                                      segment_id,
                                                      field_id.get(),
                                                      DataType::VARCHAR,
                                                      DataType::NONE,
                                                      true);
    auto index_meta = gen_index_meta(
        segment_id, field_id.get(), index_build_id, index_version);

    auto storage_config = gen_local_storage_config(TestLocalPath);
    auto cm = CreateChunkManager(storage_config);
    auto fs = storage::InitArrowFileSystem(storage_config);

    auto field_data =
        storage::CreateFieldData(DataType::VARCHAR, DataType::NONE, true);
    field_data->FillFieldData(
        values.data(), valid_bitmap.data(), values.size(), 0);

    auto payload_reader =
        std::make_shared<milvus::storage::PayloadReader>(field_data);
    storage::InsertData insert_data(payload_reader);
    insert_data.SetFieldDataMeta(field_meta);
    insert_data.SetTimestamps(0, 100);
    auto serialized_bytes = insert_data.Serialize(storage::Remote);

    auto log_path = fmt::format("{}{}/{}/{}/{}/{}",
                                TestLocalPath,
                                collection_id,
                                partition_id,
                                segment_id,
                                field_id.get(),
                                0);
    auto cm_w = ChunkManagerWrapper(cm);
    cm_w.Write(log_path, serialized_bytes.data(), serialized_bytes.size());

    storage::FileManagerContext ctx(field_meta, index_meta, cm, fs);
    std::vector<std::string> index_files;
    {
        Config config;
        config[milvus::index::INDEX_TYPE] = milvus::index::FMINDEX_INDEX_TYPE;
        config[INSERT_FILES_KEY] = std::vector<std::string>{log_path};
        index::FMIndexParams params{false, 32};
        auto index = std::make_shared<index::FMIndex>(ctx, params);
        index->Build(config);
        index_files = index->UploadUnified({})->GetIndexFiles();
    }

    Config config;
    config[milvus::index::INDEX_FILES] = index_files;
    config[milvus::LOAD_PRIORITY] = milvus::proto::common::LoadPriority::HIGH;
    config[milvus::index::ENABLE_MMAP] = false;
    index::FMIndexParams params{true, 32};
    auto idx = std::make_unique<index::FMIndex>(ctx, params);
    idx->LoadUnified(config);

    EXPECT_EQ(idx->Count(), 4);
    // empty string matches the empty row (1) only, NOT the null row (2)
    std::string empty = "";
    EXPECT_EQ(SetBits(idx->In(1, &empty)), (std::vector<int64_t>{1}));
    // null-ness is carried separately: only row 2 is null
    EXPECT_EQ(SetBits(idx->IsNull()), (std::vector<int64_t>{2}));
    EXPECT_EQ(SetBits(idx->IsNotNull()), (std::vector<int64_t>{0, 1, 3}));

    // `LIKE '%'` (empty literal) matches every NON-NULL row and excludes the
    // null row (2), matching IsNotNull() — including the genuine empty-string
    // row (1).
    EXPECT_EQ(SetBits(idx->PatternMatch("", proto::plan::OpType::PrefixMatch)),
              (std::vector<int64_t>{0, 1, 3}));
    EXPECT_EQ(SetBits(idx->PatternMatch("", proto::plan::OpType::InnerMatch)),
              (std::vector<int64_t>{0, 1, 3}));
    EXPECT_EQ(SetBits(idx->PatternMatch("", proto::plan::OpType::PostfixMatch)),
              (std::vector<int64_t>{0, 1, 3}));
}
