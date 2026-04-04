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
#pragma once

#include <cstdint>
#include <type_traits>
#include <xsimd/xsimd.hpp>
#include "common/BitUtil.h"

namespace milvus {

// Generic fallback implementation for toBitMask when no architecture-specific
// implementation is available
template <typename T, typename A>
uint64_t
genericToBitMask(xsimd::batch_bool<T, A> mask) {
    constexpr size_t size = xsimd::batch_bool<T, A>::size;
    uint64_t result = 0;
    auto ones = xsimd::batch<T, A>(T(1));
    auto zeros = xsimd::batch<T, A>(T(0));
    auto int_batch = xsimd::select(mask, ones, zeros);
    alignas(A::alignment()) T values[size];
    int_batch.store_aligned(values);
    for (size_t i = 0; i < size; ++i) {
        if (values[i] != T(0)) {
            result |= (uint64_t(1) << i);
        }
    }
    return result;
}

template <typename T, typename A, size_t kSizeT = sizeof(T)>
struct BitMask;

// ═══════════════════════════════════════════════════════════════════════════
// sizeof(T) == 1  (int8_t / uint8_t)
// AVX512BW: __mmask64 — already a bitmask, zero-cost extract.
// AVX2:     _mm256_movemask_epi8 → 32 bits, 1 per byte lane.
// NEON:     shift-and-add per 8-byte half.
// ═══════════════════════════════════════════════════════════════════════════
template <typename T, typename A>
struct BitMask<T, A, 1> {
    static constexpr uint64_t kAllSet =
        milvus::bits::lowMask(xsimd::batch_bool<T, A>::size);

#if XSIMD_WITH_AVX512BW
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx512bw&) {
        return static_cast<uint64_t>(mask.data);
    }
#endif

#if XSIMD_WITH_AVX2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx2&) {
        return static_cast<uint32_t>(_mm256_movemask_epi8(mask));
    }
#endif

#if XSIMD_WITH_SSE2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::sse2&) {
        return static_cast<uint16_t>(_mm_movemask_epi8(mask));
    }
#endif

#if XSIMD_WITH_NEON
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::neon&) {
        // Safe NEON path: build bitmask from a stored 0/1 byte vector.
        constexpr size_t lanes = xsimd::batch_bool<T, A>::size;
        if constexpr (lanes == 16) {
            alignas(16) T values[lanes];
            auto ones = xsimd::batch<T, A>(T(1));
            auto zeros = xsimd::batch<T, A>(T(0));
            auto int_batch = xsimd::select(mask, ones, zeros);
            int_batch.store_aligned(values);

            uint8x16_t v = vld1q_u8(reinterpret_cast<const uint8_t*>(values));
            // Shift each lane by its lane index: [0..7, 0..7]
            static const int8_t kShiftArr[16] = {
                0, 1, 2, 3, 4, 5, 6, 7, 0, 1, 2, 3, 4, 5, 6, 7};
            int8x16_t shifts = vld1q_s8(kShiftArr);
            uint8x16_t shifted = vshlq_u8(v, shifts);
            // Sum halves to form 8-bit masks and combine into 16-bit result
            uint8_t low = vaddv_u8(vget_low_u8(shifted));
            uint8_t high = vaddv_u8(vget_high_u8(shifted));
            return static_cast<uint64_t>(low) |
                   (static_cast<uint64_t>(high) << 8);
        } else {
            return genericToBitMask(mask);
        }
    }
#endif

    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::generic&) {
        return genericToBitMask(mask);
    }
};

// ═══════════════════════════════════════════════════════════════════════════
// sizeof(T) == 2  (int16_t)
// AVX512BW: __mmask32 — direct extract.
// AVX2:     movemask_epi8 → extract every 2nd bit.
// NEON:     narrow 16→8, then shift-and-add.
// ═══════════════════════════════════════════════════════════════════════════
template <typename T, typename A>
struct BitMask<T, A, 2> {
    static constexpr uint64_t kAllSet =
        milvus::bits::lowMask(xsimd::batch_bool<T, A>::size);

#if XSIMD_WITH_AVX512BW
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx512bw&) {
        return static_cast<uint64_t>(mask.data);
    }
#endif

#if XSIMD_WITH_AVX2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx2&) {
        return bits::extractBits<uint32_t>(_mm256_movemask_epi8(mask),
                                           0xAAAAAAAA);
    }
#endif

#if XSIMD_WITH_SSE2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::sse2&) {
        return milvus::bits::extractBits<uint32_t>(_mm_movemask_epi8(mask),
                                                   0xAAAA);
    }
#endif

#if XSIMD_WITH_NEON
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::neon&) {
        // Safe NEON path for 8 lanes (int16_t):
        // 1) convert to 0/1 u16 values via select+store
        // 2) narrow to u8, shift by lane index [0..7], horizontal add
        alignas(16) T values[8];
        auto ones = xsimd::batch<T, A>(T(1));
        auto zeros = xsimd::batch<T, A>(T(0));
        auto int_batch = xsimd::select(mask, ones, zeros);
        int_batch.store_aligned(values);

        uint16x8_t v16 = vld1q_u16(reinterpret_cast<const uint16_t*>(values));
        uint8x8_t v8 = vqmovn_u16(v16);
        static const int8_t kShift8[8] = {0, 1, 2, 3, 4, 5, 6, 7};
        int8x8_t shifts = vld1_s8(kShift8);
        uint8x8_t shifted = vshl_u8(v8, shifts);
        uint8_t mask8 = vaddv_u8(shifted);
        return static_cast<uint64_t>(mask8);
    }
#endif

    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::generic&) {
        return genericToBitMask(mask);
    }
};

// ═══════════════════════════════════════════════════════════════════════════
// sizeof(T) == 4  (int32_t / float)
// AVX512F:  __mmask16 — direct extract (only needs Foundation, not BW).
// AVX:      _mm256_movemask_ps → 8 bits, 1 per float lane.
// NEON:     generic fallback (batch_bool cast unavailable).
// ═══════════════════════════════════════════════════════════════════════════
template <typename T, typename A>
struct BitMask<T, A, 4> {
    static constexpr uint64_t kAllSet =
        milvus::bits::lowMask(xsimd::batch_bool<T, A>::size);

#if XSIMD_WITH_AVX512F
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx512f&) {
        return static_cast<uint64_t>(mask.data);
    }
#endif

#if XSIMD_WITH_AVX
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx&) {
        if constexpr (std::is_same_v<decltype(mask.data), __m256>) {
            return _mm256_movemask_ps(mask.data);
        } else {
            return _mm256_movemask_ps(_mm256_castsi256_ps(mask.data));
        }
    }
#endif

#if XSIMD_WITH_SSE2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::sse2&) {
        if constexpr (std::is_same_v<decltype(mask.data), __m128>) {
            return _mm_movemask_ps(mask.data);
        } else {
            return _mm_movemask_ps(_mm_castsi128_ps(mask.data));
        }
    }
#endif

#if XSIMD_WITH_NEON
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::neon&) {
        return genericToBitMask(mask);
    }
#endif

    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::generic&) {
        return genericToBitMask(mask);
    }
};

// ═══════════════════════════════════════════════════════════════════════════
// sizeof(T) == 8  (int64_t / double)
// AVX512F:  __mmask8 — direct extract (only needs Foundation, not BW).
// AVX:      _mm256_movemask_pd → 4 bits, 1 per double lane.
// NEON:     generic fallback (batch_bool cast unavailable).
// ═══════════════════════════════════════════════════════════════════════════
template <typename T, typename A>
struct BitMask<T, A, 8> {
    static constexpr uint64_t kAllSet =
        milvus::bits::lowMask(xsimd::batch_bool<T, A>::size);

#if XSIMD_WITH_AVX512F
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx512f&) {
        return static_cast<uint64_t>(mask.data);
    }
#endif

#if XSIMD_WITH_AVX
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::avx&) {
        if constexpr (std::is_same_v<decltype(mask.data), __m256d>) {
            return _mm256_movemask_pd(mask.data);
        } else {
            return _mm256_movemask_pd(_mm256_castsi256_pd(mask.data));
        }
    }
#endif

#if XSIMD_WITH_SSE2
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::sse2&) {
        if constexpr (std::is_same_v<decltype(mask.data), __m128d>) {
            return _mm_movemask_pd(mask.data);
        } else {
            return _mm_movemask_pd(_mm_castsi128_pd(mask.data));
        }
    }
#endif

#if XSIMD_WITH_NEON
    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::neon&) {
        // Safe NEON path for 2 lanes (int64/double):
        // 1) convert to 0/1 via select+store
        // 2) shift lane 1 by 1, OR both lanes
        alignas(16) T values[2];
        auto ones = xsimd::batch<T, A>(T(1));
        auto zeros = xsimd::batch<T, A>(T(0));
        auto int_batch = xsimd::select(mask, ones, zeros);
        int_batch.store_aligned(values);

        uint64x2_t v01;
        if constexpr (std::is_floating_point_v<T>) {
            float64x2_t vf = vld1q_f64(reinterpret_cast<const double*>(values));
            // vf > 0.0 -> all ones, else zeros; normalize to 0/1 by >>63
            uint64x2_t vm =
                vreinterpretq_u64_u64(vcgtq_f64(vf, vdupq_n_f64(0.0)));
            v01 = vshrq_n_u64(vm, 63);
        } else {
            v01 = vld1q_u64(reinterpret_cast<const uint64_t*>(values));
        }
        static const int64_t kShiftArr2[2] = {0, 1};
        int64x2_t shifts = vld1q_s64(kShiftArr2);
        uint64x2_t shifted = vshlq_u64(v01, shifts);
        return vgetq_lane_u64(shifted, 0) | vgetq_lane_u64(shifted, 1);
    }
#endif

    static uint64_t
    toBitMask(xsimd::batch_bool<T, A> mask, const xsimd::generic&) {
        return genericToBitMask(mask);
    }
};

// ═══════════════════════════════════════════════════════════════════════════
// Free-function dispatcher — selects best overload for the default arch.
// ═══════════════════════════════════════════════════════════════════════════
template <typename T, typename A = xsimd::default_arch>
uint64_t
toBitMask(xsimd::batch_bool<T, A> mask, const A& arch = {}) {
    return BitMask<T, A>::toBitMask(mask, arch);
}
}  // namespace milvus
