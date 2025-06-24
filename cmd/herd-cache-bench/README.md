# Herd Cache Benchmark Tool

A streamlined benchmark tool comparing JSON parsing vs SQLite with denormalized columns for Herd cache files.

## Quick Start

```bash
# Build for Linux
GOOS=linux GOARCH=amd64 go build -o herd-cache-bench-linux-amd64 main_clean.go

# Optimal performance test (recommended)
./herd-cache-bench-linux-amd64 --sqlite-only --use-jsoniter -f 'app-role=github-worker'

# Compare JSON vs SQLite
./herd-cache-bench-linux-amd64 --use-jsoniter -f 'app-role=github-worker'
```

## Performance Results

**Expected speedup**: 500-1000x faster queries with SQLite denormalized columns vs JSON parsing.

```
Testing denorm_filter: 580µs (711 results)     ← SQLite with denormalized columns
Testing json_extract:  477ms (711 results)     ← JSON indexes (820x slower)
```

## Usage

### Basic Options

```bash
--sqlite-only           # Skip JSON benchmark, test SQLite only (faster)
--use-jsoniter          # Use jsoniter instead of stdlib (11% faster parsing)
--force-rebuild         # Delete and rebuild database from scratch
-f, --filter           # Filter hosts by attribute (e.g., 'app-role=worker')
```

### Index Comparison Modes

```bash
--json-index           # Create JSON indexes for comparison with denormalized columns
--fts-index            # Create FTS index for experimental fuzzy search
```

### Multi-attribute Filters

```bash
-f 'app-role=worker,region=us-east,site=prod'    # Multiple filters supported
```

## Examples

### 1. Quick Performance Test
```bash
# Fast test with optimal settings
./herd-cache-bench-linux-amd64 --sqlite-only --use-jsoniter -f 'app-role=worker'
```

### 2. Compare Indexing Strategies
```bash
# Test denormalized columns vs JSON indexes
./herd-cache-bench-linux-amd64 --sqlite-only --use-jsoniter --json-index -f 'app-role=worker'
```

### 3. Full JSON vs SQLite Comparison
```bash
# Compare current JSON approach with SQLite
./herd-cache-bench-linux-amd64 --use-jsoniter -f 'app-role=worker'
```

### 4. Experiment with Full-Text Search
```bash
# Test FTS performance (slower but handles any attribute)
./herd-cache-bench-linux-amd64 --sqlite-only --use-jsoniter --fts-index -f 'app-role=worker'
```

### 5. Multi-attribute Queries
```bash
# Test complex filtering
./herd-cache-bench-linux-amd64 --sqlite-only --use-jsoniter -f 'app-role=worker,region=us-east'
```

## Output Explanation

### SQLite-Only Mode
```
=== SQLite Import (Ephemeral Mode) ===
- Data import: 3.8s        ← Pure data insertion time
- Index creation: 1.4s     ← Pure index creation time  
- Total time: 5.2s
- Denormalized indexes: 245ms    ← Fast column-based indexes
- JSON indexes: 1.2s             ← Slower function-based indexes

=== SQLite Query Benchmark ===
Testing denorm_filter: 580µs (196 results)    ← Winner: denormalized columns
Testing json_extract: 477ms (196 results)     ← Comparison: JSON indexes
Testing fts_search: 14ms (196 results)        ← Experimental: full-text search
```

### Performance Summary
```
JSON parsing: 3.383s      ← Current approach
SQLite query: 580µs       ← New approach  
Speedup: 5833x faster with SQLite
```

## Architecture

### Hybrid Denormalization Strategy
- **Hot attributes** (top 6): Stored as dedicated columns with fast indexes
- **Cold attributes**: Stored in JSON blob for flexibility
- **Best of both worlds**: Fast queries + storage efficiency

### Schema
```sql
CREATE TABLE hosts (
    id INTEGER PRIMARY KEY,
    name TEXT,
    address TEXT,
    provider TEXT,
    attributes_json TEXT,    -- Fallback for rare attributes
    -- Hot path denormalized columns
    app_role TEXT,           -- Most common filter
    region TEXT,
    site TEXT,
    app TEXT,
    role TEXT,
    stamp TEXT
);
```

### Index Strategy
```sql
-- Fast denormalized column indexes
CREATE INDEX idx_app_role ON hosts(app_role);
CREATE INDEX idx_app_role_region ON hosts(app_role, region);  -- Compound queries

-- Optional: JSON function indexes (for comparison)
CREATE INDEX idx_json_app_role ON hosts(json_extract(attributes_json, '$.app-role'));
```

## Smart Database Management

The tool automatically:
- **Detects missing indexes** and rebuilds database when needed
- **Reuses existing database** when indexes match test mode
- **Creates appropriate indexes** based on flags used

```bash
# First run - creates database
./herd-cache-bench-linux-amd64 --sqlite-only --json-index -f 'test'
# Output: Database missing required indexes, rebuilding...

# Second run - reuses database  
./herd-cache-bench-linux-amd64 --sqlite-only --json-index -f 'test'
# Output: Using existing SQLite database

# Different mode - auto-rebuilds
./herd-cache-bench-linux-amd64 --sqlite-only --fts-index -f 'test'  
# Output: Database missing required indexes, rebuilding...
```

## Requirements

- Go 1.19+ (for building)
- Herd cache files in `~/.cache/herd/` (or specify custom directory)
- Linux/macOS (SQLite with FTS5 support)

## Building

```bash
# Local build
go build -o herd-cache-bench main_clean.go

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o herd-cache-bench-linux-amd64 main_clean.go

# Cross-compile for macOS ARM64  
GOOS=darwin GOARCH=arm64 go build -o herd-cache-bench-darwin-arm64 main_clean.go
```

## Expected Performance

Based on production testing with 40K+ hosts:

| Operation | JSON Time | SQLite Time | Speedup |
|-----------|-----------|-------------|---------|
| Count all hosts | ~3.8s | 1.1ms | **3,400x** |
| Single attribute filter | ~3.0s | 0.58ms | **5,200x** |
| Multi-attribute filter | ~3.0s | 0.58ms | **5,200x** |
| Complex queries | ~3.0s | 1-2ms | **1,500-3,000x** |

**Storage**: 93MB JSON → 27-334MB SQLite (depending on indexes)
**Break-even**: After 3-4 queries (one-time conversion cost vs massive query speedup)

## Troubleshooting

### No cache files found
```bash
# Specify custom cache directory
./herd-cache-bench-linux-amd64 /path/to/cache/dir --sqlite-only -f 'test'
```

### Permission denied
```bash
# Database stored in cache directory, ensure write permissions
ls -la ~/.cache/herd/
```

### FTS not working
```bash
# Force rebuild FTS index
./herd-cache-bench-linux-amd64 --sqlite-only --fts-index --force-rebuild -f 'test'
```

## Production Recommendation

For production Herd deployment:

1. **Use hybrid denormalization** (this tool's approach)
2. **Enable jsoniter** (11% parsing improvement)
3. **Index top 10 attributes** as denormalized columns
4. **Keep JSON fallback** for rare attributes

Expected result: **Sub-millisecond query times** vs current 3+ second response times.