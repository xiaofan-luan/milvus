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

// Benchmark for `LIKE '%P%'` (InnerMatch) over VARCHAR, comparing:
//   - FMINDEX : milvus::index::FMIndex — backward search + enumerate, EXACT.
//   - brute   : per-row std::string::find over the raw corpus (the executor's
//               non-indexed data path).
//
// Corpus: 1,000,000 rows x ~1000 bytes each (~1 GiB text), independent random
// text per row. Selectivity is controlled by injecting a distinct 6-char
// marker into a fixed fraction of rows, so a single infix literal hits exactly
// 1% / 10% / 99% of rows.
//
// Expected shape: brute is ~constant in selectivity (touches every byte of
// every row); FM enumerates matches, O(occ x sa_sample_rate) — wins by orders
// of magnitude at low selectivity and converges to (slightly above) the scan
// as the match set approaches 100% — the crossover the count-first guard
// (PR-2) exists to arbitrate.
//
// NGRAM was deliberately dropped from this harness: its build cost and index
// size swing by ORDERS OF MAGNITUDE with the corpus' gram diversity (7 MiB /
// minutes on a shared-filler corpus; >18 GiB and 3.5 h WITHOUT finishing on
// this random corpus, where nearly every 2-4 gram is unique and tantivy
// degenerates into per-178-doc segment flushes plus an unbounded merge). No
// single corpus yields a number that generalizes, so an in-tree comparison
// would mislead; FM's build (~15 min) and size (~1.3x text) depend only on
// text LENGTH. See the design doc's capability matrix for the qualitative
// NGRAM comparison (candidates + recheck vs exact).

#include <benchmark/benchmark.h>
#include <execinfo.h>

#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <map>
#include <memory>
#include <random>
#include <string>
#include <utility>
#include <vector>

#include "cachinglayer/Manager.h"
#include "common/FieldData.h"
#include "common/Types.h"
#include "common/common_type_c.h"
#include "exec/expression/function/init_c.h"
#include "folly/init/Init.h"
#include "index/FMIndex.h"
#include "index/IndexInfo.h"
#include "index/IndexStats.h"
#include "index/Meta.h"
#include "pb/plan.pb.h"
#include "segcore/arrow_fs_c.h"
#include "storage/FileManager.h"
#include "storage/LocalChunkManagerSingleton.h"
#include "storage/MmapManager.h"
#include "storage/RemoteChunkManagerSingleton.h"
#include "storage/Types.h"
#include "storage/Util.h"
#include "test_utils/DataGen.h"
#include "test_utils/storage_test_utils.h"

// test_utils/Constants.h declares these extern; init_gtest.cpp defines them for
// the gtest binary, which this benchmark does not link. Provide our own defs and
// set them in main().
std::string TestLocalPath = "/tmp/fm_bench/local_data/";
std::string TestRemotePath = "/tmp/fm_bench/remote_data/";
std::string TestMmapPath = "/tmp/fm_bench/mmap_data/";

using namespace milvus;

namespace {

// ---- corpus: 1M rows x ~1000 bytes, controlled infix selectivity ----

constexpr size_t kRows = 1000000;
constexpr size_t kRowBytes = 1000;
constexpr uint32_t kSampleRate = 8;   // FM production default

// Distinct 6-char markers, none a substring of the lowercase filler or of each
// other. Injected in the row's middle so each is a true infix:
//   "AAAAAA" -> rows where i % 100 == 0        -> 1%
//   "BBBBBB" -> rows where i % 10  == 0         -> 10%
//   "CCCCCC" -> rows where i % 100 != 0         -> 99%
const std::string kPat01 = "AAAAAA";
const std::string kPat10 = "BBBBBB";
const std::string kPat99 = "CCCCCC";

struct Corpus {
    std::vector<std::string> rows;
    size_t n = 0;
};

Corpus
GenCorpus() {
    Corpus c;
    c.n = kRows;
    c.rows.reserve(kRows);

    // Every row gets its OWN random lowercase+digit text — a realistic
    // alphabet and no cross-row duplication (a shared filler would make the
    // corpus pathologically compressible and flatter any index). Uppercase is
    // reserved for the markers so they never occur by accident.
    std::mt19937_64 rng(0x9E3779B97F4A7C15ull);
    static const char kAlpha[] = "abcdefghijklmnopqrstuvwxyz0123456789";
    constexpr size_t kAlphaN = sizeof(kAlpha) - 1;
    const size_t mid = kRowBytes / 2;

    std::string row;
    for (size_t i = 0; i < kRows; i++) {
        row.clear();
        row.reserve(kRowBytes + 24);
        // 8 random chars per 64-bit draw keeps generation ~O(GB/s).
        while (row.size() < kRowBytes) {
            uint64_t r = rng();
            for (int k = 0; k < 8 && row.size() < kRowBytes; k++) {
                row.push_back(kAlpha[(r & 0xFF) % kAlphaN]);
                r >>= 8;
            }
        }
        // Splice markers into the middle (independent injections; a row may
        // carry several). Insertion keeps each marker a clean infix.
        std::string ins;
        if (i % 100 == 0) {
            ins += kPat01;
        }
        if (i % 10 == 0) {
            ins += kPat10;
        }
        if (i % 100 != 0) {
            ins += kPat99;
        }
        if (!ins.empty()) {
            row.insert(mid, ins);
        }
        c.rows.push_back(row);
    }
    return c;
}

// ---- brute-force scan baseline (the non-indexed data path) ----
// Produces the same TargetBitmap the executor's data path would, so both
// methods are timed on producing the identical result object.

TargetBitmap
BruteInnerBitmap(const std::vector<std::string>& rows, const std::string& p) {
    TargetBitmap bm(rows.size());
    for (size_t i = 0; i < rows.size(); i++) {
        if (rows[i].find(p) != std::string::npos) {
            bm.set(i);
        }
    }
    return bm;
}

// ---- FMINDEX: Build -> Upload -> mmap Load ----

struct BuiltFM {
    std::unique_ptr<index::FMIndex> idx;
    int64_t serialized_bytes = 0;
};

BuiltFM
BuildLoadFM(const std::vector<std::string>& rows, int tag) {
    const int64_t collection_id = 1, partition_id = 2, segment_id = 3;
    const int64_t field_id = 100 + tag;
    const int64_t index_build_id = 5000 + tag, index_version = 5000 + tag;

    char rootbuf[96];
    std::snprintf(rootbuf, sizeof(rootbuf), "/tmp/fm_bench/fm_%d/", tag);
    auto storage_config = gen_local_storage_config(std::string(rootbuf));
    auto cm = milvus::storage::CreateChunkManager(storage_config);
    auto fs = milvus::storage::InitArrowFileSystem(storage_config);

    auto field_meta = milvus::segcore::gen_field_meta(collection_id,
                                                      partition_id,
                                                      segment_id,
                                                      field_id,
                                                      DataType::VARCHAR,
                                                      DataType::NONE,
                                                      /*nullable=*/false,
                                                      /*max_length=*/2048);
    auto index_meta =
        gen_index_meta(segment_id, field_id, index_build_id, index_version);
    storage::FileManagerContext ctx(field_meta, index_meta, cm, fs);

    auto field_data =
        storage::CreateFieldData(DataType::VARCHAR, DataType::NONE, false);
    field_data->FillFieldData(rows.data(), rows.size());

    BuiltFM result;
    index::FMIndexParams params{/*loading_index=*/false, kSampleRate};
    auto builder = std::make_shared<index::FMIndex>(ctx, params);
    builder->BuildWithFieldData({field_data});
    auto stats = builder->UploadUnified({});
    result.serialized_bytes = stats->GetSerializedSize();

    Config cfg;
    cfg[milvus::index::INDEX_FILES] = stats->GetIndexFiles();
    cfg[milvus::LOAD_PRIORITY] = milvus::proto::common::LoadPriority::HIGH;
    cfg[milvus::index::ENABLE_MMAP] = true;
    index::FMIndexParams lparams{/*loading_index=*/true, kSampleRate};
    result.idx = std::make_unique<index::FMIndex>(ctx, lparams);
    result.idx->LoadUnified(cfg);
    return result;
}

int64_t
FmCount(const TargetBitmap& bm) {
    int64_t c = 0;
    for (size_t i = 0; i < bm.size(); i++) {
        if (bm[i]) {
            c++;
        }
    }
    return c;
}

// ---- shared fixture: corpus + both indexes, built once ----

struct Fixture {
    Corpus corpus;
    BuiltFM fm;
    double fm_build_s = 0;
};

Fixture&
GetFixture() {
    // Intentionally leaked: the fixture holds indexes whose destructors touch
    // storage singletons (chunk managers, tantivy), and static-destruction
    // order across TUs/singletons at process exit is undefined — a
    // namespace-scope `static Fixture` crashed at teardown for exactly that
    // reason. A process-lifetime fixture's correct disposal is the process
    // exit itself.
    static Fixture* f = [] {
        auto* fx = new Fixture();
        fx->corpus = GenCorpus();
        auto t0 = std::chrono::steady_clock::now();
        fx->fm = BuildLoadFM(fx->corpus.rows, 1);
        auto t1 = std::chrono::steady_clock::now();
        fx->fm_build_s = std::chrono::duration<double>(t1 - t0).count();
        std::fprintf(stderr,
                     "[fixture] rows=%zu row_bytes=%zu FM_index=%.1f MiB "
                     "(build %.1f s)\n",
                     fx->corpus.n,
                     kRowBytes,
                     fx->fm.serialized_bytes / (1024.0 * 1024.0),
                     fx->fm_build_s);
        return fx;
    }();
    return *f;
}

void
SetCounters(benchmark::State& state,
            const Fixture& f,
            const std::string& pat,
            int64_t matches) {
    state.counters["matches"] = static_cast<double>(matches);
    state.counters["match_pct"] =
        100.0 * static_cast<double>(matches) / static_cast<double>(f.corpus.n);
    state.counters["rows"] = static_cast<double>(f.corpus.n);
    state.counters["FM_MiB"] =
        f.fm.serialized_bytes / (1024.0 * 1024.0);
}

}  // namespace

// ============ InnerMatch (LIKE '%P%') at 1% / 10% / 99% selectivity ==========

// Each Run times only the production of the match TargetBitmap; the match count
// (for the selectivity counter) is computed once, outside the timed loop.

static void
RunFM(benchmark::State& state, const std::string& pat) {
    auto& f = GetFixture();
    for (auto _ : state) {
        const auto& bm =
            f.fm.idx->PatternMatch(pat, proto::plan::OpType::InnerMatch);
        benchmark::DoNotOptimize(bm);
    }
    SetCounters(
        state,
        f,
        pat,
        FmCount(f.fm.idx->PatternMatch(pat, proto::plan::OpType::InnerMatch)));
}

static void
RunBrute(benchmark::State& state, const std::string& pat) {
    auto& f = GetFixture();
    for (auto _ : state) {
        auto bm = BruteInnerBitmap(f.corpus.rows, pat);
        benchmark::DoNotOptimize(bm);
    }
    SetCounters(state, f, pat, FmCount(BruteInnerBitmap(f.corpus.rows, pat)));
}

#define BENCH2(tag, pat)                                                 \
    static void BM_##tag##_FM(benchmark::State& s) { RunFM(s, pat); }    \
    BENCHMARK(BM_##tag##_FM)->Unit(benchmark::kMicrosecond);            \
    static void BM_##tag##_Brute(benchmark::State& s) {                  \
        RunBrute(s, pat);                                                \
    }                                                                    \
    BENCHMARK(BM_##tag##_Brute)->Unit(benchmark::kMicrosecond)

BENCH2(p01, kPat01);  // 1%   selectivity
BENCH2(p10, kPat10);  // 10%  selectivity
BENCH2(p99, kPat99);  // 99%  selectivity

// ---- crash diagnostics + storage-subsystem init (mirrors init_gtest.cpp) ----

static void
crash_handler(int sig) {
    void* bt[64];
    int n = backtrace(bt, 64);
    fprintf(stderr, "\n=== signal %d backtrace (%d frames) ===\n", sig, n);
    backtrace_symbols_fd(bt, n, 2);
    _exit(128 + sig);
}

int
main(int argc, char** argv) {
    signal(SIGSEGV, crash_handler);
    signal(SIGABRT, crash_handler);
    ::benchmark::Initialize(&argc, argv);
    folly::Init follyInit(&argc, &argv, false);

    std::filesystem::create_directories(TestLocalPath);
    std::filesystem::create_directories(TestRemotePath);
    std::filesystem::create_directories(TestMmapPath);

    InitExecExpressionFunctionFactory();
    milvus::storage::LocalChunkManagerSingleton::GetInstance().Init(
        TestLocalPath);
    milvus::storage::RemoteChunkManagerSingleton::GetInstance().Init(
        get_default_local_storage_config());
    milvus::storage::MmapManager::GetInstance().Init(get_default_mmap_config());

    CStorageConfig arrow_fs_config = {};
    arrow_fs_config.root_path = TestLocalPath.c_str();
    arrow_fs_config.storage_type = "local";
    auto c_status = InitArrowFileSystem(arrow_fs_config);
    if (c_status.error_code != 0) {
        fprintf(stderr, "Failed to init arrow filesystem\n");
        return 1;
    }

    static const int64_t mb = 1024 * 1024;
    milvus::cachinglayer::Manager::ConfigureTieredStorage(
        {CacheWarmupPolicy::CacheWarmupPolicy_Disable,
         CacheWarmupPolicy::CacheWarmupPolicy_Disable,
         CacheWarmupPolicy::CacheWarmupPolicy_Disable,
         CacheWarmupPolicy::CacheWarmupPolicy_Disable},
        {1024 * mb, 1024 * mb, 1024 * mb, 1024 * mb, 1024 * mb, 1024 * mb},
        true,
        true,
        {10, true, 30},
        std::chrono::milliseconds(0),
        std::chrono::milliseconds(-1));

    milvus::index::kOverrideRootPathForUT = "files";

    ::benchmark::RunSpecifiedBenchmarks();
    ::benchmark::Shutdown();
    return 0;
}
