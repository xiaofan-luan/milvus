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

#include <cstring>
#include <functional>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

#include "common/Consts.h"
#include "common/FieldData.h"
#include "common/Schema.h"
#include "common/Types.h"
#include "index/FMIndex.h"
#include "index/IndexInfo.h"
#include "index/Meta.h"
#include "common/type_c.h"
#include "pb/common.pb.h"
#include "pb/plan.pb.h"
#include "pb/schema.pb.h"
#include "plan/PlanNode.h"
#include "query/ExecPlanNodeVisitor.h"
#include "query/PlanProto.h"
#include "segcore/SegmentSealed.h"
#include "segcore/Types.h"
#include "segcore/load_index_c.h"
#include "storage/FileManager.h"
#include "storage/InsertData.h"
#include "storage/PayloadReader.h"
#include "storage/RemoteChunkManagerSingleton.h"
#include "storage/Types.h"
#include "storage/Util.h"
#include "test_utils/Constants.h"
#include "test_utils/DataGen.h"
#include "test_utils/GenExprProto.h"
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

    // The allowlist mirrors UnaryIndexFunc's dispatch: these five ops resolve
    // to In/NotIn/PatternMatch, all answered exactly.
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::Equal));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::NotEqual));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PrefixMatch));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PostfixMatch));
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch));

    // Everything else declines (falls back to the raw-data scan) — wrongly
    // accepting would route into Range()/PatternMatch overloads that throw.
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::GreaterThan));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::GreaterEqual));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::LessThan));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::LessEqual));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::Match));
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::RegexMatch));

    // And the ops it declines to route would indeed throw if reached directly.
    EXPECT_THROW(idx->Range("m", OpType::GreaterThan), SegcoreError);
}

// Count-first guard, folded into ShouldUseOp(op, pattern): for the anchored
// pattern ops with a literal in hand, accelerate iff
// occ x sa_sample_rate < kFMIndexCostRatio x total_tokens. With 1000 rows x
// ~500 B (~500k tokens) and the test helper's sa_sample_rate = 32, the
// decline threshold is occ >= 500k x 0.001 / 32 ~= 15.6 occurrences.
TEST(FMIndex, CountFirstGuardDeclinesHighHitPatterns) {
    std::vector<std::string> data;
    std::string filler(500, 'x');  // marker letters never occur in the filler
    data.reserve(1000);
    for (int i = 0; i < 1000; i++) {
        std::string row = filler;
        if (i % 200 == 0) {
            row += "RARE";  // 5 rows -> occ 5, well under the threshold
        }
        if (i % 2 == 0) {
            row += "COMMON";  // 500 rows -> occ 500, well over
        }
        data.push_back(std::move(row));
    }
    auto idx = MakeRawDataIndex(data);

    // Low-hit pattern: index wins, guard accelerates.
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch, "RARE"));
    // High-hit pattern (50% of rows): enumeration loses to the scan, decline.
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch, "COMMON"));
    // Degenerate single-char pattern (~500k occurrences): decline.
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch, "x"));
    // Absent pattern: occ = 0, cheapest possible index answer — accelerate.
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::InnerMatch, "QQQQ"));
    // Empty literal (LIKE '%', or a caller without literal information):
    // always accelerate — answered by an O(rows) bitmap clone.
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PrefixMatch, ""));
    // Uncounted/declined op: the literal cannot rescue it.
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::Match, "zz"));
    // Anchored variants count docs, not occurrences.
    EXPECT_FALSE(idx->ShouldUseOp(proto::plan::OpType::PrefixMatch,
                                  "x"));  // every row starts with x
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::PostfixMatch, "RARE"));
    // Equal/NotEqual accelerate regardless of literal (In/NotIn path).
    EXPECT_TRUE(idx->ShouldUseOp(proto::plan::OpType::Equal, "COMMON"));
}

// Library-level: a LoadView'd (zero-copy) index must answer doc queries with
// nothing but the mapped bytes, and Extract — the only consumer of the lazily
// built ISA table — must still work after the view-load (first call builds it).
TEST(FMIndex, LibraryLoadViewZeroCopyAndLazyExtract) {
    std::vector<std::string> data{"apple", "banana", "grape"};
    std::vector<std::string_view> docs(data.begin(), data.end());
    index::fmindex::FMIndex built;
    built.Build(docs, /*sa_sample_rate=*/4);
    std::string blob = built.Serialize();

    // LoadView requires 8-byte alignment; std::string does not guarantee it.
    std::vector<uint64_t> aligned((blob.size() + 7) / 8);
    std::memcpy(aligned.data(), blob.data(), blob.size());
    auto viewed = index::fmindex::FMIndex::LoadView(
        reinterpret_cast<const uint8_t*>(aligned.data()), blob.size());
    ASSERT_TRUE(viewed.valid());

    auto p = [](const char* s) {
        return reinterpret_cast<const uint8_t*>(s);
    };
    EXPECT_EQ(viewed.MatchingDocs(p("an"), 2), (std::vector<uint64_t>{1}));
    EXPECT_EQ(viewed.CountPrefixDocs(p("gr"), 2), 1u);
    // Extract triggers the lazy ISA build on the viewed index.
    EXPECT_EQ(viewed.Extract(0, 0, 5), "apple");
    EXPECT_EQ(viewed.Extract(1, 2, 4), "nana");
    // Round-trip: re-serializing the viewed index reproduces the blob.
    EXPECT_EQ(viewed.Serialize(), blob);
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

// ---- executor path: declined ops downgrade to the scan, accepted ops agree --

// Run real filter expressions through ExecuteQueryExpr on a sealed segment that
// has BOTH the raw VARCHAR column and a loaded FMINDEX. This pins the routing
// contract at the execution layer, not just at ShouldUseOp's return value:
//   - ops the index declines (range `>`/`<=`, general LIKE `a%e`) must
//     transparently fall back to the raw-data scan — correct rows, NO throw;
//   - ops it accepts (anchored LIKE, equality, `LIKE '%'` lowered to
//     PrefixMatch("")) must return exactly the scan's rows.
// Note on the count-first guard: on this tiny corpus (~50 tokens) the guard
// declines every non-empty anchored pattern (a scan of 50 bytes always beats
// enumeration), so the anchored cases below ALSO exercise the guard-declined
// RawData path end-to-end; the empty-literal case (always accelerated) keeps
// the index path covered. Either path must produce identical rows — that
// invariance is exactly what this test pins.
TEST(FMIndex, ExecutorPathDeclinedOpsFallBackToScan) {
    int64_t collection_id = 1, partition_id = 2, segment_id = 3;
    int64_t index_build_id = 6001, index_version = 6001, index_id = 7001;

    auto schema = std::make_shared<Schema>();
    auto field_id = schema->AddDebugField("fm_exec", DataType::VARCHAR);

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

    std::vector<std::string> data{
        "apple", "banana", "grape", "melon", "zebra", "apse", "ane", ""};
    const size_t nb = data.size();

    auto field_data =
        storage::CreateFieldData(DataType::VARCHAR, DataType::NONE, false);
    field_data->FillFieldData(data.data(), data.size());

    // Sealed segment with the raw column loaded (the scan path's data source).
    auto segment = milvus::segcore::CreateSealedSegment(schema);
    auto field_data_info = PrepareSingleFieldInsertBinlog(collection_id,
                                                          partition_id,
                                                          segment_id,
                                                          field_id.get(),
                                                          {field_data},
                                                          cm);
    segment->LoadFieldData(field_data_info);

    // Binlog for the index build.
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
                                1);
    auto cm_w = ChunkManagerWrapper(cm);
    cm_w.Write(log_path, serialized_bytes.data(), serialized_bytes.size());

    storage::FileManagerContext ctx(field_meta, index_meta, cm, fs);
    std::vector<std::string> index_files;
    int64_t index_size = 0;
    {
        Config config;
        config[milvus::index::INDEX_TYPE] = milvus::index::FMINDEX_INDEX_TYPE;
        config[INSERT_FILES_KEY] = std::vector<std::string>{log_path};
        index::FMIndexParams params{/*loading_index=*/false,
                                    /*sa_sample_rate=*/8};
        auto built = std::make_shared<index::FMIndex>(ctx, params);
        built->Build(config);
        auto stats = built->UploadUnified({});
        index_size = stats->GetSerializedSize();
        index_files = stats->GetIndexFiles();
    }

    // Load the index INTO the segment through the production entry point
    // (AppendIndexV2 -> IndexFactory -> LoadUnified), so queries route through
    // the real pinned-index path.
    std::map<std::string, std::string> index_params{
        {milvus::index::INDEX_TYPE, milvus::index::FMINDEX_INDEX_TYPE},
        {milvus::index::FM_SA_SAMPLE_RATE, "8"},
        {milvus::LOAD_PRIORITY, "HIGH"},
        {milvus::index::SCALAR_INDEX_ENGINE_VERSION, "3"},
    };
    milvus::segcore::LoadIndexInfo load_index_info{};
    load_index_info.collection_id = collection_id;
    load_index_info.partition_id = partition_id;
    load_index_info.segment_id = segment_id;
    load_index_info.field_id = field_id.get();
    load_index_info.field_type = DataType::VARCHAR;
    load_index_info.enable_mmap = true;
    load_index_info.mmap_dir_path = TestLocalPath + "mmap";
    load_index_info.index_id = index_id;
    load_index_info.index_build_id = index_build_id;
    load_index_info.index_version = index_version;
    load_index_info.index_params = index_params;
    load_index_info.index_files = index_files;
    load_index_info.schema = field_meta.field_schema;
    load_index_info.index_size = index_size;
    uint8_t trace_id[16] = {1};
    uint8_t span_id[8] = {2};
    CTraceContext trace{};
    trace.traceID = trace_id;
    trace.spanID = span_id;
    trace.traceFlags = 0;
    AppendIndexV2(trace, static_cast<CLoadIndexInfo>(&load_index_info));
    segment->LoadIndex(load_index_info);

    auto run = [&](proto::plan::OpType op, std::string value) {
        auto* unary = test::GenUnaryRangeExpr(op, value);
        unary->set_allocated_column_info(test::GenColumnInfo(
            field_id.get(), proto::schema::DataType::VarChar, false, false));
        auto expr = test::GenExpr();
        expr->set_allocated_unary_range_expr(unary);
        auto parser = milvus::query::ProtoParser(schema);
        auto typed_expr = parser.ParseExprs(*expr);
        auto node = std::make_shared<plan::FilterBitsNode>(DEFAULT_PLANNODE_ID,
                                                           typed_expr);
        return milvus::query::ExecuteQueryExpr(
            node, segment.get(), nb, MAX_TIMESTAMP);
    };
    auto expect_rows = [&](const BitsetType& got,
                           const std::function<bool(const std::string&)>& oracle,
                           const char* what) {
        for (size_t i = 0; i < nb; i++) {
            EXPECT_EQ(got[i], oracle(data[i]))
                << what << " row " << i << " (" << data[i] << ")";
        }
    };

    // DECLINED: lexicographic range — must scan, not throw.
    expect_rows(
        run(proto::plan::OpType::GreaterThan, "m"),
        [](const std::string& s) { return s > "m"; },
        "GreaterThan");
    expect_rows(
        run(proto::plan::OpType::LessEqual, "banana"),
        [](const std::string& s) { return s <= "banana"; },
        "LessEqual");
    // DECLINED: general LIKE 'a%e' (interior wildcard -> Match) — regex scan.
    expect_rows(
        run(proto::plan::OpType::Match, "a%e"),
        [](const std::string& s) {
            return !s.empty() && s.front() == 'a' && s.back() == 'e';
        },
        "Match a%e");
    // ACCEPTED: anchored ops answered by the index, must equal the scan.
    expect_rows(
        run(proto::plan::OpType::InnerMatch, "an"),
        [](const std::string& s) { return s.find("an") != std::string::npos; },
        "InnerMatch an");
    expect_rows(
        run(proto::plan::OpType::PrefixMatch, "ap"),
        [](const std::string& s) { return s.rfind("ap", 0) == 0; },
        "PrefixMatch ap");
    expect_rows(
        run(proto::plan::OpType::Equal, "banana"),
        [](const std::string& s) { return s == "banana"; },
        "Equal banana");
    // ACCEPTED: `LIKE '%'` lowers to PrefixMatch("") — all rows (no nulls here),
    // the empty-pattern fix exercised end-to-end.
    expect_rows(
        run(proto::plan::OpType::PrefixMatch, ""),
        [](const std::string&) { return true; },
        "PrefixMatch empty");
}
