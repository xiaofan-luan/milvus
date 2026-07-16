// Licensed to the LF AI & Data foundation under Apache-2.0.
// 2-bit-packed vector with SWAR "count 2-bit lanes == v" rank + 2-level
// directory. Words may be OWNED (built in RAM) or VIEWED (a pointer into an
// mmap'd, 8-byte-aligned buffer, zero-copy). The directory is always owned and
// rebuilt on load. Building block of the 4-ary quad wavelet matrix.
#pragma once
#include <array>
#include <cstddef>
#include <cstdint>
#include <utility>
#include <vector>

namespace milvus::index::fmindex {

class QuadVector {
 public:
    QuadVector() = default;

    explicit QuadVector(const std::vector<uint8_t>& syms)
        : n_(syms.size()), owned_words_((syms.size() + 31) / 32, 0ULL) {
        for (size_t i = 0; i < n_; ++i) {
            owned_words_[i >> 5] |= (uint64_t(syms[i] & 3) << (2 * (i & 31)));
        }
        words_ = owned_words_.data();
        nwords_ = owned_words_.size();
        build_rank();
    }

    QuadVector(const QuadVector&) = delete;
    QuadVector&
    operator=(const QuadVector&) = delete;
    QuadVector(QuadVector&& o) noexcept {
        *this = std::move(o);
    }
    QuadVector&
    operator=(QuadVector&& o) noexcept {
        n_ = o.n_;
        nwords_ = o.nwords_;
        owned_words_ = std::move(o.owned_words_);
        sb_ = std::move(o.sb_);
        rel_ = std::move(o.rel_);
        words_ = owned_words_.empty() ? o.words_ : owned_words_.data();
        return *this;
    }

    size_t
    rank(uint8_t v, size_t i) const {
        size_t b = i >> 5;
        size_t sb = b >> kSbShift;
        size_t off = i & 31;
        size_t r = sb_[sb][v] + rel_[b][v];
        if (off) {
            r += swar_count(words_[b], v, off);
        }
        return r;
    }

    uint8_t
    at(size_t i) const {
        return (words_[i >> 5] >> (2 * (i & 31))) & 3u;
    }

    void
    prefetch(size_t i) const {
        size_t w = i >> 5;
        if (w >= nwords_) {
            w = nwords_ ? nwords_ - 1 : 0;
        }
        __builtin_prefetch(&rel_[i >> 5], 0, 3);
        if (nwords_) {
            __builtin_prefetch(&words_[w], 0, 3);
        }
    }

    size_t
    size() const {
        return n_;
    }
    size_t
    word_count() const {
        return nwords_;
    }
    const uint64_t*
    words() const {
        return words_;
    }

    static QuadVector
    from_words(size_t n, std::vector<uint64_t> words) {
        QuadVector q;
        q.owned_words_ = std::move(words);
        q.n_ = n;
        q.words_ = q.owned_words_.data();
        q.nwords_ = q.owned_words_.size();
        q.build_rank();
        return q;
    }

    static QuadVector
    from_view(size_t n, const uint64_t* words, size_t nwords) {
        QuadVector q;
        q.n_ = n;
        q.words_ = words;
        q.nwords_ = nwords;
        q.build_rank();
        return q;
    }

 private:
    static constexpr size_t kBlocksPerSb = 64;  // 64 words = 2048 symbols
    static constexpr size_t kSbShift = 6;

    static size_t
    swar_count(uint64_t word, uint8_t v, size_t k) {
        constexpr uint64_t kLow = 0x5555555555555555ULL;
        uint64_t t = word ^ (uint64_t(v) * kLow);
        uint64_t eq = (~t) & (~(t >> 1)) & kLow;
        if (k < 32) {
            eq &= (1ULL << (2 * k)) - 1;
        }
        return __builtin_popcountll(eq);
    }

    void
    build_rank() {
        const size_t nblocks = nwords_;  // 1 word (32 symbols) per block
        const size_t nsb = (nblocks + kBlocksPerSb - 1) / kBlocksPerSb;
        sb_.assign(nsb + 1, {0, 0, 0, 0});
        rel_.assign(nblocks + 1, {0, 0, 0, 0});
        std::array<uint64_t, 4> acc{0, 0, 0, 0};
        for (size_t b = 0; b <= nblocks; ++b) {
            if ((b & (kBlocksPerSb - 1)) == 0) {
                sb_[b >> kSbShift] = acc;
            }
            const auto& sbase = sb_[b >> kSbShift];
            for (uint8_t v = 0; v < 4; ++v) {
                rel_[b][v] = static_cast<uint16_t>(acc[v] - sbase[v]);
            }
            if (b < nblocks) {
                size_t base = b * 32;
                size_t valid = n_ - base < 32 ? n_ - base : 32;
                for (uint8_t v = 0; v < 4; ++v) {
                    acc[v] += swar_count(words_[b], v, valid);
                }
            }
        }
    }

    size_t n_ = 0;
    const uint64_t* words_ = nullptr;
    size_t nwords_ = 0;
    std::vector<uint64_t> owned_words_;         // non-empty iff owning
    std::vector<std::array<uint64_t, 4>> sb_;   // owned (rebuilt)
    std::vector<std::array<uint16_t, 4>> rel_;  // owned (rebuilt)
};

}  // namespace milvus::index::fmindex
