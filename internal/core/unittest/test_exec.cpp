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

#include <stddef.h>
#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <iostream>
#include <map>
#include <memory>
#include <string>
#include <unordered_map>
#include <vector>

#include "NamedType/named_type_impl.hpp"
#include "bitset/common.h"
#include "common/Common.h"
#include "common/Consts.h"
#include "common/FieldData.h"
#include "common/FieldDataInterface.h"
#include "common/Schema.h"
#include "common/Types.h"
#include "common/Vector.h"
#include "common/protobuf_utils.h"
#include "exec/QueryContext.h"
#include "exec/Task.h"
#include "exec/expression/ConjunctExpr.h"
#include "exec/expression/Expr.h"
#include "exec/expression/function/FunctionFactory.h"
#include "expr/ITypeExpr.h"
#include "gtest/gtest.h"
#include "index/NgramInvertedIndex.h"
#include "index/SkipIndex.h"
#include "knowhere/comp/index_param.h"
#include "pb/plan.pb.h"
#include "plan/PlanNode.h"
#include "query/PlanNode.h"
#include "query/Utils.h"
#include "segcore/ChunkedSegmentSealedImpl.h"
#include "segcore/SegcoreConfig.h"
#include "segcore/SegmentSealed.h"
#include "storage/RemoteChunkManagerSingleton.h"
#include "storage/Util.h"
#include "test_utils/DataGen.h"
#include "test_utils/storage_test_utils.h"

using namespace milvus;
using namespace milvus::exec;
using namespace milvus::query;
using namespace milvus::segcore;

class TaskTest : public testing::TestWithParam<DataType> {
 protected:
    void
    SetUp() override {
        using namespace milvus;
        using namespace milvus::query;
        using namespace milvus::segcore;
        milvus::exec::expression::FunctionFactory& factory =
            milvus::exec::expression::FunctionFactory::Instance();
        factory.Initialize();

        auto schema = std::make_shared<Schema>();
        schema->AddDebugField("fakevec", GetParam(), 16, knowhere::metric::L2);
        auto bool_fid = schema->AddDebugField("bool", DataType::BOOL);
        field_map_.insert({"bool", bool_fid});
        auto bool_1_fid = schema->AddDebugField("bool1", DataType::BOOL);
        field_map_.insert({"bool1", bool_1_fid});
        auto int8_fid = schema->AddDebugField("int8", DataType::INT8);
        field_map_.insert({"int8", int8_fid});
        auto int8_1_fid = schema->AddDebugField("int81", DataType::INT8);
        field_map_.insert({"int81", int8_1_fid});
        auto int16_fid = schema->AddDebugField("int16", DataType::INT16);
        field_map_.insert({"int16", int16_fid});
        auto int16_1_fid = schema->AddDebugField("int161", DataType::INT16);
        field_map_.insert({"int161", int16_1_fid});
        auto int32_fid = schema->AddDebugField("int32", DataType::INT32);
        field_map_.insert({"int32", int32_fid});
        auto int32_1_fid = schema->AddDebugField("int321", DataType::INT32);
        field_map_.insert({"int321", int32_1_fid});
        auto int64_fid = schema->AddDebugField("int64", DataType::INT64);
        field_map_.insert({"int64", int64_fid});
        auto int64_1_fid = schema->AddDebugField("int641", DataType::INT64);
        field_map_.insert({"int641", int64_1_fid});
        auto float_fid = schema->AddDebugField("float", DataType::FLOAT);
        field_map_.insert({"float", float_fid});
        auto float_1_fid = schema->AddDebugField("float1", DataType::FLOAT);
        field_map_.insert({"float1", float_1_fid});
        auto double_fid = schema->AddDebugField("double", DataType::DOUBLE);
        field_map_.insert({"double", double_fid});
        auto double_1_fid = schema->AddDebugField("double1", DataType::DOUBLE);
        field_map_.insert({"double1", double_1_fid});
        auto str1_fid = schema->AddDebugField("string1", DataType::VARCHAR);
        field_map_.insert({"string1", str1_fid});
        auto str2_fid = schema->AddDebugField("string2", DataType::VARCHAR);
        field_map_.insert({"string2", str2_fid});
        auto str3_fid = schema->AddDebugField("string3", DataType::VARCHAR);
        field_map_.insert({"string3", str3_fid});
        auto json_fid = schema->AddDebugField("json", DataType::JSON);
        field_map_.insert({"json", json_fid});
        schema->set_primary_field_id(str1_fid);

        size_t N = 100000;
        num_rows_ = N;
        auto raw_data = DataGen(schema, N);
        auto segment = CreateSealedWithFieldDataLoaded(schema, raw_data);
        segment_ = SegmentSealedSPtr(segment.release());
    }

    void
    TearDown() override {
    }

 public:
    SegmentSealedSPtr segment_;
    std::map<std::string, FieldId> field_map_;
    int64_t num_rows_{0};
};

INSTANTIATE_TEST_SUITE_P(TaskTestSuite,
                         TaskTest,
                         ::testing::Values(DataType::VECTOR_FLOAT,
                                           DataType::VECTOR_SPARSE_U32_F32));

TEST_P(TaskTest, RegisterFunction) {
    milvus::exec::expression::FunctionFactory& factory =
        milvus::exec::expression::FunctionFactory::Instance();
    ASSERT_EQ(factory.GetFilterFunctionNum(), 2);
    auto all_functions = factory.ListAllFilterFunctions();
    // for (auto& f : all_functions) {
    //     std::cout << f.toString() << std::endl;
    // }

    auto func_ptr = factory.GetFilterFunction(
        milvus::exec::expression::FilterFunctionRegisterKey{
            "empty", {DataType::VARCHAR}});
    ASSERT_TRUE(func_ptr != nullptr);
}

TEST_P(TaskTest, CallExprEmpty) {
    expr::ColumnInfo col(field_map_["string1"], DataType::VARCHAR);
    std::vector<milvus::expr::TypedExprPtr> parameters;
    parameters.push_back(std::make_shared<milvus::expr::ColumnExpr>(col));
    milvus::exec::expression::FunctionFactory& factory =
        milvus::exec::expression::FunctionFactory::Instance();
    auto empty_function_ptr = factory.GetFilterFunction(
        milvus::exec::expression::FilterFunctionRegisterKey{
            "empty", {DataType::VARCHAR}});
    auto call_expr = std::make_shared<milvus::expr::CallExpr>(
        "empty", parameters, empty_function_ptr);
    ASSERT_EQ(call_expr->inputs().size(), 1);
    std::vector<milvus::plan::PlanNodePtr> sources;
    auto filter_node = std::make_shared<milvus::plan::FilterBitsNode>(
        "plannode id 1", call_expr, sources);
    auto plan = plan::PlanFragment(filter_node);
    auto query_context = std::make_shared<milvus::exec::QueryContext>(
        "test1",
        segment_.get(),
        100000,
        MAX_TIMESTAMP,
        0,
        0,
        query::PlanOptions{false},
        std::make_shared<milvus::exec::QueryConfig>(
            std::unordered_map<std::string, std::string>{}));

    auto start = std::chrono::steady_clock::now();
    auto task = Task::Create("task_call_expr_empty", plan, 0, query_context);
    int64_t num_rows = 0;
    for (;;) {
        auto result = task->Next();
        if (!result) {
            break;
        }
        num_rows += result->size();
    }
    auto cost = std::chrono::duration_cast<std::chrono::microseconds>(
                    std::chrono::steady_clock::now() - start)
                    .count();
    std::cout << "cost: " << cost << "us" << std::endl;
    EXPECT_EQ(num_rows, num_rows_);
}

TEST_P(TaskTest, UnaryExpr) {
    ::milvus::proto::plan::GenericValue value;
    value.set_int64_val(-1);
    auto logical_expr = std::make_shared<milvus::expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(field_map_["int64"], DataType::INT64),
        proto::plan::OpType::LessThan,
        value,
        std::vector<proto::plan::GenericValue>{});
    std::vector<milvus::plan::PlanNodePtr> sources;
    auto filter_node = std::make_shared<milvus::plan::FilterBitsNode>(
        "plannode id 1", logical_expr, sources);
    auto plan = plan::PlanFragment(filter_node);
    auto query_context = std::make_shared<milvus::exec::QueryContext>(
        "test1",
        segment_.get(),
        100000,
        MAX_TIMESTAMP,
        0,
        0,
        query::PlanOptions{false},
        std::make_shared<milvus::exec::QueryConfig>(
            std::unordered_map<std::string, std::string>{}));

    auto start = std::chrono::steady_clock::now();
    auto task = Task::Create("task_unary_expr", plan, 0, query_context);
    int64_t num_rows = 0;
    for (;;) {
        auto result = task->Next();
        if (!result) {
            break;
        }
        num_rows += result->size();
    }
    auto cost = std::chrono::duration_cast<std::chrono::microseconds>(
                    std::chrono::steady_clock::now() - start)
                    .count();
    std::cout << "cost: " << cost << "us" << std::endl;
    EXPECT_EQ(num_rows, num_rows_);
}

TEST_P(TaskTest, LogicalExpr) {
    ::milvus::proto::plan::GenericValue value;
    value.set_int64_val(-1);
    auto left = std::make_shared<milvus::expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(field_map_["int64"], DataType::INT64),
        proto::plan::OpType::LessThan,
        value,
        std::vector<proto::plan::GenericValue>{});
    auto right = std::make_shared<milvus::expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(field_map_["int64"], DataType::INT64),
        proto::plan::OpType::LessThan,
        value,
        std::vector<proto::plan::GenericValue>{});

    auto top = std::make_shared<milvus::expr::LogicalBinaryExpr>(
        expr::LogicalBinaryExpr::OpType::And, left, right);
    std::vector<milvus::plan::PlanNodePtr> sources;
    auto filter_node = std::make_shared<milvus::plan::FilterBitsNode>(
        "plannode id 1", top, sources);
    auto plan = plan::PlanFragment(filter_node);
    auto query_context = std::make_shared<milvus::exec::QueryContext>(
        "test1",
        segment_.get(),
        100000,
        MAX_TIMESTAMP,
        0,
        0,
        query::PlanOptions{false},
        std::make_shared<milvus::exec::QueryConfig>(
            std::unordered_map<std::string, std::string>{}));

    auto start = std::chrono::steady_clock::now();
    auto task =
        Task::Create("task_logical_binary_expr", plan, 0, query_context);
    int64_t num_rows = 0;
    for (;;) {
        auto result = task->Next();
        if (!result) {
            break;
        }
        num_rows += result->size();
    }
    auto cost = std::chrono::duration_cast<std::chrono::microseconds>(
                    std::chrono::steady_clock::now() - start)
                    .count();
    std::cout << "cost: " << cost << "us" << std::endl;
    EXPECT_EQ(num_rows, num_rows_);
}

TEST_P(TaskTest, Test_reorder) {
    using namespace milvus;
    using namespace milvus::query;
    using namespace milvus::segcore;
    using namespace milvus::exec;

    {
        // expr:  string2 like '%xx' and string2 == 'xxx'
        // reorder: string2 == "xxx" and string2 like '%xxx'
        proto::plan::GenericValue val1;
        val1.set_string_val("%xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["string2"], DataType::VARCHAR),
            proto::plan::OpType::Match,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue val2;
        val2.set_string_val("xxx");
        auto expr2 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["string2"], DataType::VARCHAR),
            proto::plan::OpType::Equal,
            val2,
            std::vector<proto::plan::GenericValue>{});
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr:  string2 == 'xxx' and int1 < 100
        // reorder: int1 < 100 and string2 == 'xxx'
        proto::plan::GenericValue val1;
        val1.set_string_val("xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["string2"], DataType::VARCHAR),
            proto::plan::OpType::Equal,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue val2;
        val2.set_int64_val(100);
        auto expr2 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["int64"], DataType::INT64),
            proto::plan::OpType::LessThan,
            val2,
            std::vector<proto::plan::GenericValue>{});
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr: json['b'] like '%xx' and json['a'] == 'xxx'
        // reorder: json['a'] == 'xxx' and json['b'] like '%xx'
        proto::plan::GenericValue val1;
        val1.set_string_val("%xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["json"], DataType::JSON),
            proto::plan::OpType::Match,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue val2;
        val2.set_string_val("xxx");
        auto expr2 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["json"], DataType::JSON),
            proto::plan::OpType::Equal,
            val2,
            std::vector<proto::plan::GenericValue>{});
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr: json['a'] == 'xxx' and int1 ==  100
        // reorder: int1 == 100 and json['a'] == 'xxx'
        proto::plan::GenericValue val1;
        val1.set_string_val("xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["json"], DataType::JSON),
            proto::plan::OpType::Equal,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue val2;
        val2.set_int64_val(100);
        auto expr2 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["int64"], DataType::INT64),
            proto::plan::OpType::Equal,
            val2,
            std::vector<proto::plan::GenericValue>{});
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr: json['a'] == 'xxx' and 0 < int1 < 100
        // reorder:  0 < int1 < 100 and json['a'] == 'xxx'
        proto::plan::GenericValue val1;
        val1.set_string_val("xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["json"], DataType::JSON),
            proto::plan::OpType::Equal,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue low;
        low.set_int64_val(0);
        proto::plan::GenericValue upper;
        upper.set_int64_val(100);
        auto expr2 = std::make_shared<expr::BinaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["int64"], DataType::INT64),
            low,
            upper,
            false,
            false);
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr: string1 != string2 and 0 < int1 < 100
        // reorder:  0 < int1 < 100 and string1 != string2
        proto::plan::GenericValue val1;
        val1.set_string_val("xxx");
        auto expr1 = std::make_shared<expr::CompareExpr>(field_map_["string1"],
                                                         field_map_["string2"],
                                                         DataType::VARCHAR,
                                                         DataType::VARCHAR,
                                                         OpType::LessThan);
        proto::plan::GenericValue low;
        low.set_int64_val(0);
        proto::plan::GenericValue upper;
        upper.set_int64_val(100);
        auto expr2 = std::make_shared<expr::BinaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["int64"], DataType::INT64),
            low,
            upper,
            false,
            false);
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 2);
        EXPECT_EQ(reorder[0], 1);
        EXPECT_EQ(reorder[1], 0);
    }

    {
        // expr:  string2 like '%xx' and string2 == 'xxx'
        // disable optimize expr, still remain sequence
        proto::plan::GenericValue val1;
        val1.set_string_val("%xxx");
        auto expr1 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["string2"], DataType::VARCHAR),
            proto::plan::OpType::Match,
            val1,
            std::vector<proto::plan::GenericValue>{});
        proto::plan::GenericValue val2;
        val2.set_string_val("xxx");
        auto expr2 = std::make_shared<expr::UnaryRangeFilterExpr>(
            expr::ColumnInfo(field_map_["string2"], DataType::VARCHAR),
            proto::plan::OpType::Equal,
            val2,
            std::vector<proto::plan::GenericValue>{});
        auto expr3 = std::make_shared<expr::LogicalBinaryExpr>(
            expr::LogicalBinaryExpr::OpType::And, expr1, expr2);
        auto query_context = std::make_shared<milvus::exec::QueryContext>(
            DEAFULT_QUERY_ID, segment_.get(), 100000, MAX_TIMESTAMP);
        ExecContext context(query_context.get());
        OPTIMIZE_EXPR_ENABLED.store(false);
        auto exprs =
            milvus::exec::CompileExpressions({expr3}, &context, {}, false);
        EXPECT_EQ(exprs.size(), 1);
        EXPECT_STREQ(exprs[0]->name().c_str(), "PhyConjunctFilterExpr");
        auto phy_expr =
            std::static_pointer_cast<milvus::exec::PhyConjunctFilterExpr>(
                exprs[0]);
        std::cout << phy_expr->ToString() << std::endl;
        auto reorder = phy_expr->GetReorder();
        EXPECT_EQ(reorder.size(), 0);
        OPTIMIZE_EXPR_ENABLED.store(true, std::memory_order_release);
    }
}

// This test verifies the fix for https://github.com/milvus-io/milvus/issues/46053.
//
// Bug scenario:
// - Expression: string_field == "target" AND int64_field == X AND float_field > Y
// - Data is stored in multiple chunks
// - SkipIndex skips some chunks for the float range condition
// - Expression reordering: numeric expressions execute before string expressions
// - When a chunk is skipped, processed_cursor in execute_sub_batch wasn't updated
// - This caused bitmap_input indices to be misaligned for subsequent expressions
//
// The fix ensures that when a chunk is skipped by SkipIndex, we still call
// func(nullptr, ...) so that execute_sub_batch can update its internal cursors.
TEST(TaskTest, SkipIndexWithBitmapInputAlignment) {
    using namespace milvus;
    using namespace milvus::query;
    using namespace milvus::segcore;
    using namespace milvus::exec;

    auto schema = std::make_shared<Schema>();
    auto dim = 4;
    auto metrics_type = "L2";
    auto fake_vec_fid = schema->AddDebugField(
        "fakeVec", DataType::VECTOR_FLOAT, dim, metrics_type);
    auto pk_fid = schema->AddDebugField("pk", DataType::INT64);
    schema->set_primary_field_id(pk_fid);
    auto string_fid = schema->AddDebugField("string_field", DataType::VARCHAR);
    auto int64_fid = schema->AddDebugField("int64_field", DataType::INT64);
    auto float_fid = schema->AddDebugField("float_field", DataType::FLOAT);

    auto segment = CreateSealedSegment(schema);
    auto cm = milvus::storage::RemoteChunkManagerSingleton::GetInstance()
                  .GetRemoteChunkManager();

    // Create two chunks with different data distributions:
    // Chunk 0: float values [10, 20, 30, 40, 50] - will be SKIPPED by float > 60
    // Chunk 1: float values [65, 70, 75, 80, 85] - will NOT be skipped
    //
    // We place the target row (string="target_value", int64=999) in chunk 1 at index 2
    // with float=75 which satisfies float > 60

    const size_t chunk_size = 5;

    // Chunk 0: floats that will cause this chunk to be skipped (max=50 < 60)
    std::vector<float> floats_chunk0 = {10.0f, 20.0f, 30.0f, 40.0f, 50.0f};
    auto float_field_data_0 = storage::CreateFieldData(
        DataType::FLOAT, DataType::NONE, false, 1, chunk_size);
    float_field_data_0->FillFieldData(floats_chunk0.data(), chunk_size);

    // Chunk 1: floats that will NOT be skipped (min=65 > 60)
    std::vector<float> floats_chunk1 = {65.0f, 70.0f, 75.0f, 80.0f, 85.0f};
    auto float_field_data_1 = storage::CreateFieldData(
        DataType::FLOAT, DataType::NONE, false, 1, chunk_size);
    float_field_data_1->FillFieldData(floats_chunk1.data(), chunk_size);

    auto float_load_info =
        PrepareSingleFieldInsertBinlog(kCollectionID,
                                       kPartitionID,
                                       kSegmentID,
                                       float_fid.get(),
                                       {float_field_data_0, float_field_data_1},
                                       cm);
    segment->LoadFieldData(float_load_info);

    // Int64 field - target value 999 at chunk 1 index 2
    std::vector<int64_t> int64s_chunk0 = {1, 2, 3, 4, 5};
    auto int64_field_data_0 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    int64_field_data_0->FillFieldData(int64s_chunk0.data(), chunk_size);

    std::vector<int64_t> int64s_chunk1 = {6, 7, 999, 9, 10};  // 999 at index 2
    auto int64_field_data_1 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    int64_field_data_1->FillFieldData(int64s_chunk1.data(), chunk_size);

    auto int64_load_info =
        PrepareSingleFieldInsertBinlog(kCollectionID,
                                       kPartitionID,
                                       kSegmentID,
                                       int64_fid.get(),
                                       {int64_field_data_0, int64_field_data_1},
                                       cm);
    segment->LoadFieldData(int64_load_info);

    // String field - target value "target_value" at chunk 1 index 2
    std::vector<std::string> strings_chunk0 = {"a", "b", "c", "d", "e"};
    auto string_field_data_0 = storage::CreateFieldData(
        DataType::VARCHAR, DataType::NONE, false, 1, chunk_size);
    string_field_data_0->FillFieldData(strings_chunk0.data(), chunk_size);

    std::vector<std::string> strings_chunk1 = {
        "f", "g", "target_value", "i", "j"};
    auto string_field_data_1 = storage::CreateFieldData(
        DataType::VARCHAR, DataType::NONE, false, 1, chunk_size);
    string_field_data_1->FillFieldData(strings_chunk1.data(), chunk_size);

    auto string_load_info = PrepareSingleFieldInsertBinlog(
        kCollectionID,
        kPartitionID,
        kSegmentID,
        string_fid.get(),
        {string_field_data_0, string_field_data_1},
        cm);
    segment->LoadFieldData(string_load_info);

    // PK field
    std::vector<int64_t> pks_chunk0 = {100, 101, 102, 103, 104};
    auto pk_field_data_0 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    pk_field_data_0->FillFieldData(pks_chunk0.data(), chunk_size);

    std::vector<int64_t> pks_chunk1 = {105, 106, 107, 108, 109};
    auto pk_field_data_1 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    pk_field_data_1->FillFieldData(pks_chunk1.data(), chunk_size);

    auto pk_load_info =
        PrepareSingleFieldInsertBinlog(kCollectionID,
                                       kPartitionID,
                                       kSegmentID,
                                       pk_fid.get(),
                                       {pk_field_data_0, pk_field_data_1},
                                       cm);
    segment->LoadFieldData(pk_load_info);

    // Vector field (required but not used in filter)
    std::vector<float> vec_chunk0(chunk_size * dim, 1.0f);
    auto vec_field_data_0 = storage::CreateFieldData(
        DataType::VECTOR_FLOAT, DataType::NONE, false, dim, chunk_size);
    vec_field_data_0->FillFieldData(vec_chunk0.data(), chunk_size);

    std::vector<float> vec_chunk1(chunk_size * dim, 2.0f);
    auto vec_field_data_1 = storage::CreateFieldData(
        DataType::VECTOR_FLOAT, DataType::NONE, false, dim, chunk_size);
    vec_field_data_1->FillFieldData(vec_chunk1.data(), chunk_size);

    auto vec_load_info =
        PrepareSingleFieldInsertBinlog(kCollectionID,
                                       kPartitionID,
                                       kSegmentID,
                                       fake_vec_fid.get(),
                                       {vec_field_data_0, vec_field_data_1},
                                       cm);
    segment->LoadFieldData(vec_load_info);

    // Row IDs
    std::vector<int64_t> row_ids_chunk0 = {0, 1, 2, 3, 4};
    auto row_ids_data_0 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    row_ids_data_0->FillFieldData(row_ids_chunk0.data(), chunk_size);

    std::vector<int64_t> row_ids_chunk1 = {5, 6, 7, 8, 9};
    auto row_ids_data_1 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    row_ids_data_1->FillFieldData(row_ids_chunk1.data(), chunk_size);

    auto row_id_load_info =
        PrepareSingleFieldInsertBinlog(kCollectionID,
                                       kPartitionID,
                                       kSegmentID,
                                       RowFieldID.get(),
                                       {row_ids_data_0, row_ids_data_1},
                                       cm);
    segment->LoadFieldData(row_id_load_info);

    // Timestamps
    std::vector<int64_t> timestamps_chunk0 = {1, 1, 1, 1, 1};
    auto ts_data_0 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    ts_data_0->FillFieldData(timestamps_chunk0.data(), chunk_size);

    std::vector<int64_t> timestamps_chunk1 = {1, 1, 1, 1, 1};
    auto ts_data_1 = storage::CreateFieldData(
        DataType::INT64, DataType::NONE, false, 1, chunk_size);
    ts_data_1->FillFieldData(timestamps_chunk1.data(), chunk_size);

    auto ts_load_info = PrepareSingleFieldInsertBinlog(kCollectionID,
                                                       kPartitionID,
                                                       kSegmentID,
                                                       TimestampFieldID.get(),
                                                       {ts_data_0, ts_data_1},
                                                       cm);
    segment->LoadFieldData(ts_load_info);

    // Build the expression:
    // string_field == "target_value" AND int64_field == 999 AND float_field > 60
    //
    // Due to expression reordering, this will execute as:
    // 1. float_field > 60 (numeric, runs first) - SkipIndex skips chunk 0
    // 2. int64_field == 999 (numeric, runs second)
    // 3. string_field == "target_value" (string, runs last)

    // string_field == "target_value"
    proto::plan::GenericValue string_val;
    string_val.set_string_val("target_value");
    auto string_expr = std::make_shared<expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(string_fid, DataType::VARCHAR),
        proto::plan::OpType::Equal,
        string_val,
        std::vector<proto::plan::GenericValue>{});

    // int64_field == 999
    proto::plan::GenericValue int64_val;
    int64_val.set_int64_val(999);
    auto int64_expr = std::make_shared<expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(int64_fid, DataType::INT64),
        proto::plan::OpType::Equal,
        int64_val,
        std::vector<proto::plan::GenericValue>{});

    // float_field > 60
    proto::plan::GenericValue float_val;
    float_val.set_float_val(60.0f);
    auto float_expr = std::make_shared<expr::UnaryRangeFilterExpr>(
        expr::ColumnInfo(float_fid, DataType::FLOAT),
        proto::plan::OpType::GreaterThan,
        float_val,
        std::vector<proto::plan::GenericValue>{});

    // Build AND expression: string_expr AND int64_expr AND float_expr
    auto and_expr1 = std::make_shared<expr::LogicalBinaryExpr>(
        expr::LogicalBinaryExpr::OpType::And, string_expr, int64_expr);
    auto and_expr2 = std::make_shared<expr::LogicalBinaryExpr>(
        expr::LogicalBinaryExpr::OpType::And, and_expr1, float_expr);

    // Verify SkipIndex is working before running the expression:
    // Check if chunk 0 can be skipped for float > 60
    auto& skip_index = segment->GetSkipIndex();
    bool chunk0_can_skip = skip_index.CanSkipUnaryRange<float>(
        float_fid, 0, proto::plan::OpType::GreaterThan, 60.0f);
    bool chunk1_can_skip = skip_index.CanSkipUnaryRange<float>(
        float_fid, 1, proto::plan::OpType::GreaterThan, 60.0f);

    // Chunk 0 should be skippable (max=50 < 60), chunk 1 should not (min=65 > 60)
    EXPECT_TRUE(chunk0_can_skip)
        << "Chunk 0 should be skippable for float > 60 (max=50)";
    EXPECT_FALSE(chunk1_can_skip)
        << "Chunk 1 should NOT be skippable for float > 60 (min=65)";

    std::vector<milvus::plan::PlanNodePtr> sources;
    auto filter_node = std::make_shared<milvus::plan::FilterBitsNode>(
        "plannode id 1", and_expr2, sources);
    auto plan = plan::PlanFragment(filter_node);

    auto query_context = std::make_shared<milvus::exec::QueryContext>(
        "test_skip_index_bitmap_alignment",
        segment.get(),
        chunk_size * 2,  // total rows
        MAX_TIMESTAMP,
        0,
        0,
        query::PlanOptions{false},
        std::make_shared<milvus::exec::QueryConfig>(
            std::unordered_map<std::string, std::string>{}));

    auto task = Task::Create("task_skip_index_bitmap", plan, 0, query_context);

    int64_t total_rows = 0;
    int64_t filtered_rows = 0;
    for (;;) {
        auto result = task->Next();
        if (!result) {
            break;
        }
        auto col_vec =
            std::dynamic_pointer_cast<ColumnVector>(result->child(0));
        if (col_vec && col_vec->IsBitmap()) {
            TargetBitmapView view(col_vec->GetRawData(), col_vec->size());
            total_rows += col_vec->size();
            filtered_rows +=
                view.count();  // These are filtered OUT (don't match)
        }
    }

    int64_t num_matched = total_rows - filtered_rows;

    // Expected result: exactly 1 row should match
    // - Row at chunk 1, index 2 (global index 7) has:
    //   - string_field = "target_value" ✓
    //   - int64_field = 999 ✓
    //   - float_field = 75 > 60 ✓
    //
    // With the bug (before fix): 0 rows would match because bitmap_input
    // indices were misaligned after chunk 0 was skipped.
    //
    // With the fix: 1 row should match correctly.
    EXPECT_EQ(num_matched, 1);
}

// ═══════════════════════════════════════════════════════════════════════════
// TermExpr SkipIndex integration test
//
// Verifies that TermExpr (IN filter) works correctly with SkipIndex on
// sealed segments with multiple chunks, for all scalar data types.
//
// Setup per type:
//   chunk 0: values [0..chunk_size)      — does NOT overlap IN list
//   chunk 1: values [chunk_size..2*chunk_size) — contains IN targets
//
// SkipIndex should allow chunk 0 to be skipped (its min/max range excludes
// all IN values), while chunk 1 must be scanned.
// ═══════════════════════════════════════════════════════════════════════════

// Helper: create a FieldData of type T, fill it, and return it.
// For vector types, pass dim > 1 so num_rows = values.size() / dim.
template <typename T>
static FieldDataPtr
MakeFieldData(DataType dt, const std::vector<T>& values, int dim = 1) {
    auto num_rows = static_cast<int64_t>(values.size()) / dim;
    auto fd =
        storage::CreateFieldData(dt, DataType::NONE, false, dim, num_rows);
    if constexpr (std::is_same_v<T, bool>) {
        std::vector<uint8_t> buf(values.size());
        for (size_t i = 0; i < values.size(); i++) buf[i] = values[i] ? 1 : 0;
        fd->FillFieldData(buf.data(), static_cast<ssize_t>(num_rows));
    } else {
        fd->FillFieldData(values.data(), static_cast<ssize_t>(num_rows));
    }
    return fd;
}

// Helper: load a field with two chunks into a sealed segment.
static void
LoadTwoChunkField(SegmentSealed* segment,
                  int64_t field_id,
                  FieldDataPtr chunk0,
                  FieldDataPtr chunk1) {
    auto cm = storage::RemoteChunkManagerSingleton::GetInstance()
                  .GetRemoteChunkManager();
    auto load_info = PrepareSingleFieldInsertBinlog(kCollectionID,
                                                    kPartitionID,
                                                    kSegmentID,
                                                    field_id,
                                                    {chunk0, chunk1},
                                                    cm);
    segment->LoadFieldData(load_info);
}

// Helper: build a TermFilterExpr with int64 IN values.
static expr::TypedExprPtr
MakeTermExprInt64(FieldId fid,
                  DataType dt,
                  const std::vector<int64_t>& values) {
    std::vector<proto::plan::GenericValue> vals;
    for (auto v : values) {
        proto::plan::GenericValue gv;
        gv.set_int64_val(v);
        vals.push_back(gv);
    }
    return std::make_shared<expr::TermFilterExpr>(expr::ColumnInfo(fid, dt),
                                                  vals);
}

static expr::TypedExprPtr
MakeTermExprFloat(FieldId fid, DataType dt, const std::vector<double>& values) {
    std::vector<proto::plan::GenericValue> vals;
    for (auto v : values) {
        proto::plan::GenericValue gv;
        gv.set_float_val(v);
        vals.push_back(gv);
    }
    return std::make_shared<expr::TermFilterExpr>(expr::ColumnInfo(fid, dt),
                                                  vals);
}

static expr::TypedExprPtr
MakeTermExprBool(FieldId fid, const std::vector<bool>& values) {
    std::vector<proto::plan::GenericValue> vals;
    for (auto v : values) {
        proto::plan::GenericValue gv;
        gv.set_bool_val(v);
        vals.push_back(gv);
    }
    return std::make_shared<expr::TermFilterExpr>(
        expr::ColumnInfo(fid, DataType::BOOL), vals);
}

static expr::TypedExprPtr
MakeTermExprString(FieldId fid, const std::vector<std::string>& values) {
    std::vector<proto::plan::GenericValue> vals;
    for (auto& v : values) {
        proto::plan::GenericValue gv;
        gv.set_string_val(v);
        vals.push_back(gv);
    }
    return std::make_shared<expr::TermFilterExpr>(
        expr::ColumnInfo(fid, DataType::VARCHAR), vals);
}

// Run a filter expression on a segment and return the number of matched rows.
static int64_t
RunFilterAndCountMatches(SegmentSealed* segment,
                         expr::TypedExprPtr expr,
                         int64_t total_rows) {
    std::vector<milvus::plan::PlanNodePtr> sources;
    auto filter_node = std::make_shared<milvus::plan::FilterBitsNode>(
        "plannode id 1", expr, sources);
    auto plan = plan::PlanFragment(filter_node);
    auto query_context = std::make_shared<milvus::exec::QueryContext>(
        "test_term_skip_index",
        segment,
        total_rows,
        MAX_TIMESTAMP,
        0,
        0,
        query::PlanOptions{false},
        std::make_shared<milvus::exec::QueryConfig>(
            std::unordered_map<std::string, std::string>{}));
    auto task = Task::Create("task_term_skip", plan, 0, query_context);

    int64_t total = 0;
    int64_t filtered = 0;
    for (;;) {
        auto result = task->Next();
        if (!result)
            break;
        auto col = std::dynamic_pointer_cast<ColumnVector>(result->child(0));
        if (col && col->IsBitmap()) {
            TargetBitmapView view(col->GetRawData(), col->size());
            total += col->size();
            filtered += view.count();
        }
    }
    // count() returns filtered-out rows; matched = total - filtered
    return total - filtered;
}

TEST(TaskTest, TermExprSkipIndexMultiType) {
    const int chunk_size = 100;
    const int total_rows = chunk_size * 2;

    auto schema = std::make_shared<Schema>();
    auto dim = 4;
    auto vec_fid =
        schema->AddDebugField("fakeVec", DataType::VECTOR_FLOAT, dim, "L2");
    auto pk_fid = schema->AddDebugField("pk", DataType::INT64);
    schema->set_primary_field_id(pk_fid);
    auto bool_fid = schema->AddDebugField("bool_field", DataType::BOOL);
    auto int8_fid = schema->AddDebugField("int8_field", DataType::INT8);
    auto int16_fid = schema->AddDebugField("int16_field", DataType::INT16);
    auto int32_fid = schema->AddDebugField("int32_field", DataType::INT32);
    auto int64_fid = schema->AddDebugField("int64_field", DataType::INT64);
    auto float_fid = schema->AddDebugField("float_field", DataType::FLOAT);
    auto double_fid = schema->AddDebugField("double_field", DataType::DOUBLE);
    auto string_fid = schema->AddDebugField("string_field", DataType::VARCHAR);

    auto segment = CreateSealedSegment(schema);

    // ── Build chunk data ──
    // chunk 0: values in [0, chunk_size), no overlap with IN list targets
    // chunk 1: values in [chunk_size, 2*chunk_size), IN targets are here

    // Bool: chunk0 = all false, chunk1 = all true
    std::vector<bool> bool_c0(chunk_size, false);
    std::vector<bool> bool_c1(chunk_size, true);

    // Int8: chunk0 = [0..49], chunk1 = [50..99]
    // IN targets: {55, 60} — only in chunk1
    std::vector<int8_t> int8_c0(chunk_size), int8_c1(chunk_size);
    for (int i = 0; i < chunk_size; i++) {
        int8_c0[i] = static_cast<int8_t>(i % 50);         // 0..49 repeated
        int8_c1[i] = static_cast<int8_t>(50 + (i % 50));  // 50..99 repeated
    }

    // Int16/32/64: chunk0 = [0..99], chunk1 = [1000..1099]
    std::vector<int16_t> i16_c0(chunk_size), i16_c1(chunk_size);
    std::vector<int32_t> i32_c0(chunk_size), i32_c1(chunk_size);
    std::vector<int64_t> i64_c0(chunk_size), i64_c1(chunk_size);
    for (int i = 0; i < chunk_size; i++) {
        i16_c0[i] = static_cast<int16_t>(i);
        i16_c1[i] = static_cast<int16_t>(1000 + i);
        i32_c0[i] = i;
        i32_c1[i] = 1000 + i;
        i64_c0[i] = i;
        i64_c1[i] = 1000 + i;
    }

    // Float/Double: chunk0 = [0.0..99.0], chunk1 = [1000.0..1099.0]
    std::vector<float> f32_c0(chunk_size), f32_c1(chunk_size);
    std::vector<double> f64_c0(chunk_size), f64_c1(chunk_size);
    for (int i = 0; i < chunk_size; i++) {
        f32_c0[i] = static_cast<float>(i);
        f32_c1[i] = 1000.0f + i;
        f64_c0[i] = static_cast<double>(i);
        f64_c1[i] = 1000.0 + i;
    }

    // String: chunk0 = "a0".."a99", chunk1 = "b0".."b99"
    std::vector<std::string> str_c0(chunk_size), str_c1(chunk_size);
    for (int i = 0; i < chunk_size; i++) {
        str_c0[i] = "a" + std::to_string(i);
        str_c1[i] = "b" + std::to_string(i);
    }

    // PK, row_ids, timestamps
    std::vector<int64_t> pk_c0(chunk_size), pk_c1(chunk_size);
    std::vector<int64_t> rowid_c0(chunk_size), rowid_c1(chunk_size);
    std::vector<int64_t> ts_c0(chunk_size, 1), ts_c1(chunk_size, 1);
    for (int i = 0; i < chunk_size; i++) {
        pk_c0[i] = i;
        pk_c1[i] = chunk_size + i;
        rowid_c0[i] = i;
        rowid_c1[i] = chunk_size + i;
    }

    // Vector (required but not used in filter)
    std::vector<float> vec_c0(chunk_size * dim, 0.0f);
    std::vector<float> vec_c1(chunk_size * dim, 1.0f);

    // ── Load all fields ──
    LoadTwoChunkField(segment.get(),
                      bool_fid.get(),
                      MakeFieldData<bool>(DataType::BOOL, bool_c0),
                      MakeFieldData<bool>(DataType::BOOL, bool_c1));
    LoadTwoChunkField(segment.get(),
                      int8_fid.get(),
                      MakeFieldData<int8_t>(DataType::INT8, int8_c0),
                      MakeFieldData<int8_t>(DataType::INT8, int8_c1));
    LoadTwoChunkField(segment.get(),
                      int16_fid.get(),
                      MakeFieldData<int16_t>(DataType::INT16, i16_c0),
                      MakeFieldData<int16_t>(DataType::INT16, i16_c1));
    LoadTwoChunkField(segment.get(),
                      int32_fid.get(),
                      MakeFieldData<int32_t>(DataType::INT32, i32_c0),
                      MakeFieldData<int32_t>(DataType::INT32, i32_c1));
    LoadTwoChunkField(segment.get(),
                      int64_fid.get(),
                      MakeFieldData<int64_t>(DataType::INT64, i64_c0),
                      MakeFieldData<int64_t>(DataType::INT64, i64_c1));
    LoadTwoChunkField(segment.get(),
                      float_fid.get(),
                      MakeFieldData<float>(DataType::FLOAT, f32_c0),
                      MakeFieldData<float>(DataType::FLOAT, f32_c1));
    LoadTwoChunkField(segment.get(),
                      double_fid.get(),
                      MakeFieldData<double>(DataType::DOUBLE, f64_c0),
                      MakeFieldData<double>(DataType::DOUBLE, f64_c1));
    LoadTwoChunkField(segment.get(),
                      string_fid.get(),
                      MakeFieldData<std::string>(DataType::VARCHAR, str_c0),
                      MakeFieldData<std::string>(DataType::VARCHAR, str_c1));
    LoadTwoChunkField(segment.get(),
                      pk_fid.get(),
                      MakeFieldData<int64_t>(DataType::INT64, pk_c0),
                      MakeFieldData<int64_t>(DataType::INT64, pk_c1));
    LoadTwoChunkField(
        segment.get(),
        vec_fid.get(),
        MakeFieldData<float>(DataType::VECTOR_FLOAT, vec_c0, dim),
        MakeFieldData<float>(DataType::VECTOR_FLOAT, vec_c1, dim));
    LoadTwoChunkField(segment.get(),
                      RowFieldID.get(),
                      MakeFieldData<int64_t>(DataType::INT64, rowid_c0),
                      MakeFieldData<int64_t>(DataType::INT64, rowid_c1));
    LoadTwoChunkField(segment.get(),
                      TimestampFieldID.get(),
                      MakeFieldData<int64_t>(DataType::INT64, ts_c0),
                      MakeFieldData<int64_t>(DataType::INT64, ts_c1));

    // ── Verify SkipIndex for all numeric types ──
    auto& skip_index = segment->GetSkipIndex();

    // Int8: chunk0 has [0..49], chunk1 has [50..99], IN={55,60}
    EXPECT_TRUE(skip_index.CanSkipInQuery<int8_t>(
        int8_fid, 0, std::vector<int8_t>{55, 60}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<int8_t>(
        int8_fid, 1, std::vector<int8_t>{55, 60}));

    // Int16: chunk0 has [0..99], chunk1 has [1000..1099], IN={1005,1050}
    EXPECT_TRUE(skip_index.CanSkipInQuery<int16_t>(
        int16_fid, 0, std::vector<int16_t>{1005, 1050}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<int16_t>(
        int16_fid, 1, std::vector<int16_t>{1005, 1050}));

    // Int32
    EXPECT_TRUE(skip_index.CanSkipInQuery<int32_t>(
        int32_fid, 0, std::vector<int32_t>{1005, 1050}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<int32_t>(
        int32_fid, 1, std::vector<int32_t>{1005, 1050}));

    // Int64
    EXPECT_TRUE(skip_index.CanSkipInQuery<int64_t>(
        int64_fid, 0, std::vector<int64_t>{1005, 1050}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<int64_t>(
        int64_fid, 1, std::vector<int64_t>{1005, 1050}));

    // Float
    EXPECT_TRUE(skip_index.CanSkipInQuery<float>(
        float_fid, 0, std::vector<float>{1005.0f, 1050.0f}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<float>(
        float_fid, 1, std::vector<float>{1005.0f, 1050.0f}));

    // Double
    EXPECT_TRUE(skip_index.CanSkipInQuery<double>(
        double_fid, 0, std::vector<double>{1005.0, 1050.0}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<double>(
        double_fid, 1, std::vector<double>{1005.0, 1050.0}));

    // String: chunk0 has "a0".."a99", chunk1 has "b0".."b99", IN={"b5","b50"}
    // This verifies the string_view skip-index fix (GetElementValues<string_view>
    // used to return empty, silently disabling skip for varchar sealed segments).
    EXPECT_TRUE(skip_index.CanSkipInQuery<std::string>(
        string_fid, 0, std::vector<std::string>{"b5", "b50"}));
    EXPECT_FALSE(skip_index.CanSkipInQuery<std::string>(
        string_fid, 1, std::vector<std::string>{"b5", "b50"}));

    // ── Test each data type ──

    // Bool: IN (true) → should match chunk1 only (100 rows)
    EXPECT_EQ(
        RunFilterAndCountMatches(
            segment.get(), MakeTermExprBool(bool_fid, {true}), total_rows),
        chunk_size);

    // Bool: IN (false) → should match chunk0 only (100 rows)
    EXPECT_EQ(
        RunFilterAndCountMatches(
            segment.get(), MakeTermExprBool(bool_fid, {false}), total_rows),
        chunk_size);

    // Int8: IN (55, 60) → only in chunk1 (chunk0 has 0..49)
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int8_fid, DataType::INT8, {55, 60}),
                  total_rows),
              4);  // each value appears twice (100/50=2 repeats)

    // Int16: IN (1005, 1050) → only in chunk1
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int16_fid, DataType::INT16, {1005, 1050}),
                  total_rows),
              2);

    // Int32: IN (1005, 1050) → only in chunk1
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int32_fid, DataType::INT32, {1005, 1050}),
                  total_rows),
              2);

    // Int64: IN (1005, 1050) → only in chunk1
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int64_fid, DataType::INT64, {1005, 1050}),
                  total_rows),
              2);

    // Float: IN (1005.0, 1050.0) → only in chunk1
    EXPECT_EQ(
        RunFilterAndCountMatches(
            segment.get(),
            MakeTermExprFloat(float_fid, DataType::FLOAT, {1005.0, 1050.0}),
            total_rows),
        2);

    // Double: IN (1005.0, 1050.0) → only in chunk1
    EXPECT_EQ(
        RunFilterAndCountMatches(
            segment.get(),
            MakeTermExprFloat(double_fid, DataType::DOUBLE, {1005.0, 1050.0}),
            total_rows),
        2);

    // String: IN ("b5", "b50") → only in chunk1
    EXPECT_EQ(
        RunFilterAndCountMatches(segment.get(),
                                 MakeTermExprString(string_fid, {"b5", "b50"}),
                                 total_rows),
        2);

    // Cross-chunk: IN values spanning both chunks → should find in both
    // Int32: IN (5, 1005) → one in each chunk
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int32_fid, DataType::INT32, {5, 1005}),
                  total_rows),
              2);

    // Empty result: IN values not in any chunk
    EXPECT_EQ(RunFilterAndCountMatches(
                  segment.get(),
                  MakeTermExprInt64(int32_fid, DataType::INT32, {9999, 8888}),
                  total_rows),
              0);
}
