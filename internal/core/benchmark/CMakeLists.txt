# Licensed to the LF AI & Data foundation under one
# or more contributor license agreements. See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership. The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License. You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

add_executable(fastmem_benchmark FastMemBenchmark.cpp)
target_include_directories(fastmem_benchmark PRIVATE ${CMAKE_HOME_DIRECTORY}/src)
target_link_libraries(fastmem_benchmark PRIVATE benchmark::benchmark)
install(TARGETS fastmem_benchmark DESTINATION benchmark)

# End-to-end FMINDEX scalar-index benchmark. Uses the same header-only test_utils
# harness (gen_field_meta / gen_local_storage_config / ...) as all_tests, so it
# needs the unittest include dir for "test_utils/*.h". milvus_conan_deps supplies
# the conan header dirs (arrow / protobuf / boost / ...) the milvus headers pull.
add_executable(fmindex_benchmark FMIndexBenchmark.cpp)
target_include_directories(fmindex_benchmark PRIVATE
    ${CMAKE_HOME_DIRECTORY}/src
    ${CMAKE_HOME_DIRECTORY}/unittest)
target_link_libraries(fmindex_benchmark PRIVATE
    benchmark::benchmark
    milvus_core
    milvus_conan_deps
    knowhere
    milvus-storage
    milvus-planparser-cpp
    GTest::gtest)   # test_utils/DataGen.h transitively includes <gtest/gtest.h>
if (LINUX)
    # milvus_core PUBLIC-links two static libs that both provide xxhash — conan
    # libxxhash.a and milvus-storage's Rust bridge (librust_bridge.a embeds and
    # globally re-exports XXH*). Any executable linking the milvus_core dep set
    # sees duplicate XXH* symbols. Same workaround the unit tests use
    # (unittest/CMakeLists.txt): keep the first definition. Scoped to this
    # benchmark binary only — milvus_core.so and the shipped Go binary are
    # unaffected.
    target_link_options(fmindex_benchmark PRIVATE "LINKER:--allow-multiple-definition")
endif()
# milvus-planparser-cpp is an installed .so under output/lib (not a CMake target
# in this build), so point the linker + rpath at it the same way all_tests does.
set(FM_BENCH_PLANPARSER_LIB_DIR ${CMAKE_HOME_DIRECTORY}/output/lib)
target_link_options(fmindex_benchmark PRIVATE "-L${FM_BENCH_PLANPARSER_LIB_DIR}")
set_target_properties(fmindex_benchmark PROPERTIES
    BUILD_RPATH "${FM_BENCH_PLANPARSER_LIB_DIR}"
    INSTALL_RPATH "${FM_BENCH_PLANPARSER_LIB_DIR}")
install(TARGETS fmindex_benchmark DESTINATION benchmark)
