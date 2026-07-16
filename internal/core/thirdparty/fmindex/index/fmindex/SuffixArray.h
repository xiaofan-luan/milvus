// Licensed to the LF AI & Data foundation under Apache-2.0.
// Portions translated from Lance (lance_index::scalar::fmindex), Apache-2.0.
#pragma once
#include <algorithm>
#include <cstdint>
#include <cstring>
#include <vector>

#include "libsais.h"    // Apache-2.0, vendored under third_party/libsais
#include "libsais64.h"  // int64 SA path for corpora >= 2 GiB

namespace milvus::index::fmindex {

// Suffix array via libsais (SA-IS, linear time). Input is a symbol vector
// (bytes remapped to 1..256 plus a sentinel 0, alphabet <= 257). Produces the
// standard suffix array (end-of-string smallest), matching the brute-force
// oracle. This int-alphabet entry point is kept for the unit tests; FMIndex::
// Build uses the byte entry points below (build_suffix_array_bytes / _bytes64).
// For n < 2^31.
inline std::vector<uint32_t>
build_suffix_array(const std::vector<uint32_t>& s) {
    const size_t n = s.size();
    std::vector<uint32_t> sa(n);
    if (n == 0) {
        return sa;
    }
    if (n == 1) {
        sa[0] = 0;
        return sa;
    }
    // libsais_int wants a mutable int32 text and an int32 SA buffer.
    std::vector<int32_t> t(s.begin(), s.end());
    int32_t k = 0;
    for (uint32_t v : s) {
        k = std::max(k, static_cast<int32_t>(v));
    }
    ++k;                          // alphabet size = max symbol + 1
    const int32_t fs = 6 * 1024;  // recommended free space for performance
    std::vector<int32_t> saint(n + fs);
    int32_t rc =
        libsais_int(t.data(), saint.data(), static_cast<int32_t>(n), k, fs);
    (void)rc;  // rc == 0 on success; inputs here are always valid
    for (size_t i = 0; i < n; ++i) {
        sa[i] = static_cast<uint32_t>(saint[i]);
    }
    return sa;
}

// Suffix array of a dense-symbol sequence that ALREADY carries its trailing
// sentinel (dense id 0, unique-smallest, appears once at t[m-1]). Content ids
// are in [2, sigma) and the document separator is id 1; because the remap is
// order-preserving, the id SA order equals the byte/token SA order. This is the
// v2 build path — the separator is a symbol OUTSIDE the byte alphabet, so any
// byte (including '\0') is storable and queryable. `libsais_int` documents that
// it modifies the input during construction but RESTORES it on success, so the
// caller may reuse `t` afterward (FMIndex computes the BWT from it). For m < 2^31.
inline std::vector<int32_t>
build_suffix_array_symbols32(int32_t* t, size_t m, int32_t sigma) {
    if (m <= 1) {
        return std::vector<int32_t>(m,
                                    0);  // empty or lone-sentinel: SA=[0] or []
    }
    const int32_t fs = 6 * 1024;  // recommended free space for performance
    std::vector<int32_t> sa(m + fs);
#ifdef LIBSAIS_OPENMP
    libsais_int_omp(t, sa.data(), static_cast<int32_t>(m), sigma, fs, 0);
#else
    libsais_int(t, sa.data(), static_cast<int32_t>(m), sigma, fs);
#endif
    sa.resize(m);
    return sa;
}

// 64-bit counterpart (int64 positions), for m >= 2^31 and up to 2^63. Same
// semantics and identical result values as the 32-bit path; uses libsais64_long,
// the integer-alphabet SA over 64-bit indices. 8 bytes/position, so prefer the
// 32-bit path when m < 2^31.
inline std::vector<int64_t>
build_suffix_array_symbols64(int64_t* t, size_t m, int64_t sigma) {
    if (m <= 1) {
        return std::vector<int64_t>(m, 0);
    }
    const int64_t fs = 6 * 1024;
    std::vector<int64_t> sa(m + fs);
#ifdef LIBSAIS_OPENMP
    libsais64_long_omp(t, sa.data(), static_cast<int64_t>(m), sigma, fs, 0);
#else
    libsais64_long(t, sa.data(), static_cast<int64_t>(m), sigma, fs);
#endif
    sa.resize(m);
    return sa;
}

// Suffix array of the sentinel-terminated text, built directly over BYTES with
// the byte-oriented libsais — no int32 remap of the text is materialized, which
// is the bulk of the build-memory saving. libsais treats end-of-string as
// smaller than any byte, exactly our sentinel; and because the dense byte->id
// remap is order-preserving, the byte SA order equals the id SA order. The
// returned array has length n+1: element 0 is the sentinel suffix (position n,
// always smallest), elements 1..n are the byte SA. This is identical, position
// for position, to build_suffix_array(ids . [0]).
//
// `data` must already be in the order the index sorts by (i.e. case-folded by
// the caller when building a case-insensitive index).
//
// The SA stays int32 (values are non-negative positions <= n < 2^31), so no
// second uint32 buffer is allocated: libsais writes the n byte-SA entries, then
// we shift them up by one in place and drop the sentinel (position n) into
// slot 0. Peak SA memory is one int32 buffer, not two.
inline std::vector<int32_t>
build_suffix_array_bytes(const uint8_t* data, size_t n) {
    if (n == 0) {
        return std::vector<int32_t>{0};  // just the sentinel
    }
    const int32_t fs = 6 * 1024;  // recommended free space for performance
    std::vector<int32_t> sa(n + 1 + fs);
    // Write byte-SA into sa[0..n); sa[n..n+fs] is libsais scratch. With the
    // FMINDEX_OPENMP CMake switch (defines LIBSAIS_OPENMP), the parallel entry
    // point uses all OpenMP threads (bound by OMP_NUM_THREADS); otherwise it is
    // single-threaded.
#ifdef LIBSAIS_OPENMP
    libsais_omp(data, sa.data(), static_cast<int32_t>(n), fs + 1, nullptr, 0);
#else
    libsais(data, sa.data(), static_cast<int32_t>(n), fs + 1, nullptr);
#endif
    std::memmove(sa.data() + 1, sa.data(), n * sizeof(int32_t));
    sa[0] = static_cast<int32_t>(n);  // sentinel suffix sorts first
    sa.resize(n + 1);
    return sa;
}

// 64-bit counterpart of build_suffix_array_bytes: same semantics and identical
// result values, but the SA holds int64 positions so it supports corpora up to
// 2^63 bytes (the int32 version caps at 2 GiB). Uses 8 bytes/position, so the
// caller should prefer the 32-bit path when the corpus fits in < 2^31.
inline std::vector<int64_t>
build_suffix_array_bytes64(const uint8_t* data, size_t n) {
    if (n == 0) {
        return std::vector<int64_t>{0};  // just the sentinel
    }
    const int64_t fs = 6 * 1024;
    std::vector<int64_t> sa(n + 1 + fs);
#ifdef LIBSAIS_OPENMP
    libsais64_omp(data, sa.data(), static_cast<int64_t>(n), fs + 1, nullptr, 0);
#else
    libsais64(data, sa.data(), static_cast<int64_t>(n), fs + 1, nullptr);
#endif
    std::memmove(sa.data() + 1, sa.data(), n * sizeof(int64_t));
    sa[0] = static_cast<int64_t>(n);  // sentinel suffix sorts first
    sa.resize(n + 1);
    return sa;
}

}  // namespace milvus::index::fmindex
