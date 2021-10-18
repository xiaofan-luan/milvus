# Scripts and Tools
The following scripts and commands may be used during segcore development

## code format 
- under milvus/internal/core directory
    - run `./run_clang_format .` to format cpp code
        - to call clang-format-10,  need to install `apt install clang-format-10` in advance
        - call `build-support/add_${lang}_license.sh` to add license info for cmake and cpp file
- under milvus/ directory
    - use `make cppcheck` to check format, including
        - if clang-format is executed
        - if license info is added
        - if `cpplint.py` standard meets , might need to be fixed by hand
    - `make verifier` also include functions in `make cppcheck`

## code compilation
- under milvus/internal/core folder
    - use `./build.sh -u -t Debug -o cmake-build-debug`
        - compile with unittest (`-u`)
        - compile with Debug mode (`-t Debug`)
        - output to cmake-build-debug directory (`-o cmake-build-debug`)
- You can also use clion to open the core folder, and setup compilation environment
    - Need to modify `CMake Options`, keep with the parameters consistent with the first line output './build.sh'
    
- After compilation, the following executable files deserves attention
    - ${build}/unittest/all_tests: GTest format test program，including all tests, can use `--gtest_filter=` to filter the required tests
    - ${build}/bench/all_bench: performance benchmark，based on Google Benchmark. 
  