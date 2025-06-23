# Herd Cache Benchmark Tool

This tool benchmarks different JSON parsing libraries against actual Herd cache files to help identify the best performance option for your environment.

## Tested Libraries

- **encoding/json** (stdlib) - Go's standard JSON library
- **jsoniter** - High-performance drop-in replacement
- **sonic** - ByteDance's JIT-optimized parser (x86-64 only)
- **simdjson-go** - SIMD-accelerated parser (requires AVX2+CLMUL)

## Building

From this directory:

```bash
go mod tidy
go build
```

## Usage

```bash
# Parse all hosts (like your previous test)
./herd-cache-bench --parse-only

# Filter hosts like herd list (more realistic)
./herd-cache-bench --filter "app-role=github-lowworker"

# Use streaming parsers for filtering (potential speedup)
./herd-cache-bench --filter "app-role=github-lowworker" --streaming

# Compare streaming vs non-streaming
./herd-cache-bench --filter "app-role=github-lowworker" --streaming
./herd-cache-bench --filter "app-role=github-lowworker"

# Test SQLite conversion and queries
./herd-cache-bench --sqlite --filter "app-role=github-lowworker"

# Only test SQLite queries (reuse existing DB)
./herd-cache-bench --sqlite-only --filter "app-role=github-lowworker"

# Use custom cache directory
./herd-cache-bench --filter "site=production" /path/to/cache/dir

# Show help
./herd-cache-bench --help
```

## Sample Output

```
Found 3 cache files in /home/user/.cache/herd
Total size: 45.23 MB
Filter: app-role=github-lowworker
Mode: Parse + filter hosts

System info:
- CPU: amd64
- Cores: 8
- simdjson support: true
- sonic support: true

Running benchmarks (parsing all cache files)...
--------------------------------------------------------------------------------

Iteration 1/3:
  stdlib    :       420ms (115 hosts)
  jsoniter  :       380ms (115 hosts)  
  sonic     :       290ms (115 hosts)
  simdjson  :       250ms (115 hosts)

[... more iterations ...]

================================================================================
SUMMARY (average of 3 iterations):
--------------------------------------------------------------------------------
Parser        Avg Time    Speedup   Total Hosts
--------------------------------------------------
simdjson          430ms      9.77x        20428
sonic             870ms      4.83x        20428
jsoniter          1.1s       3.82x        20428
stdlib            4.2s       1.00x        20428
```

## Notes

- The benchmark reads all cache files to warm up the file system cache before testing
- Each parser is tested multiple times and results are averaged
- If a parser doesn't support your CPU, it will show an error
- Total hosts count should be the same for all parsers (validates correctness)

## Recommendations

Based on your results:
- If simdjson works on your hardware and you control deployment, use it
- If you need broader compatibility, jsoniter provides good performance everywhere
- Sonic is fast but requires modern x86-64 CPUs
- Consider implementing runtime detection to use the fastest available parser