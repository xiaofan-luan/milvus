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

#include <gtest/gtest.h>
#include <string>
#include <string_view>
#include <vector>

#include "exec/expression/Element.h"

using milvus::exec::FlatVectorElement;
using milvus::exec::SortVectorElement;
using VT = milvus::exec::MultiElement::ValueType;

// FlatVectorElement<std::string> — verify string and string_view lookups
TEST(FlatVectorElementStringTest, StringAndStringView) {
    std::vector<std::string> vals = {"alpha", "beta", "gamma"};
    FlatVectorElement<std::string> fe(vals);

    // Exact string
    EXPECT_TRUE(fe.In(VT(std::string("alpha"))));
    EXPECT_TRUE(fe.In(VT(std::string("beta"))));
    EXPECT_FALSE(fe.In(VT(std::string("delta"))));

    // string_view bridging
    EXPECT_TRUE(fe.In(VT(std::string_view("gamma"))));
    EXPECT_FALSE(fe.In(VT(std::string_view("Gamma"))));

    // Wrong type in variant should return false
    EXPECT_FALSE(fe.In(VT(int32_t{1})));

    EXPECT_FALSE(fe.Empty());
    EXPECT_EQ(fe.Size(), vals.size());
}

// SortVectorElement<std::string> — verify add/sort and membership
TEST(SortVectorElementStringTest, AddSortAndIn) {
    std::vector<std::string> unsorted = {"world", "hello", "test", "hello"};
    SortVectorElement<std::string> se(unsorted);

    // After construction, container is sorted and deduplicated by std::binary_search semantics
    EXPECT_TRUE(se.In(VT(std::string("hello"))));
    EXPECT_TRUE(se.In(VT(std::string("world"))));
    EXPECT_TRUE(se.In(VT(std::string("test"))));
    EXPECT_FALSE(se.In(VT(std::string("none"))));

    // Wrong alternative type should fail membership
    EXPECT_FALSE(se.In(VT(int64_t{0})));

    EXPECT_FALSE(se.Empty());
    EXPECT_GE(se.Size(), 3u);
}

// FlatVectorElement<int32_t> — ensure type mismatch returns false
TEST(FlatVectorElementInt32Test, TypeMismatch) {
    FlatVectorElement<int32_t> fe(std::vector<int32_t>{1, 2, 3});
    EXPECT_TRUE(fe.In(VT(int32_t{2})));
    EXPECT_FALSE(fe.In(VT(int64_t{2})));
    EXPECT_FALSE(fe.In(VT(std::string("2"))));
}
