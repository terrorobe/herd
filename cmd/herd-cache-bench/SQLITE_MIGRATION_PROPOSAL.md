# SQLite Cache Migration Proposal

## Overview
Replace herd's current JSON file-based cache with SQLite database for improved performance and query capabilities.

## Current Architecture
- **Cache Provider**: Wrapper pattern around source providers
- **Storage**: Individual JSON files per provider (`{provider}.cache`)
- **Interface**: Clean `Cache` interface with `Load()`, `Invalidate()`, `SetCacheDir()`
- **Flow**: Provider → HostSet → JSON marshal → File write

## Proposed Architecture
- **Same Interface**: No changes to provider code or Cache interface
- **SQLite Storage**: Single database with schema: `(host, provider, attributes, updated_at)`
- **Streaming**: Provider → HostSet → Stream rows → Batch SQL insert
- **Concurrent**: SQLite WAL mode for multiple provider writes

## Key Benefits
- **Performance**: SQL queries vs in-memory filtering
- **Concurrency**: Better handling of multiple herd processes
- **Atomicity**: Transaction-based updates vs file operations
- **Scalability**: Single database vs thousands of cache files

## Implementation Plan

### Phase 1: SQLite Cache Provider
- Create `provider/sqlcache/provider.go` implementing Cache interface
- Replace file operations with SQLite operations
- Maintain freshness logic using `updated_at` timestamps

### Phase 2: Streaming Integration
- Modify `Load()` to stream individual hosts to database
- Implement prepared statements for batch inserts
- Use transactions for atomic provider updates

### Phase 3: Registry Changes
- Replace `cache.NewFromProvider()` with `sqlcache.NewFromProvider()`
- Adapt `SetCacheDir()` to accept database file path

## Schema Design
```sql
CREATE TABLE cache (
    host TEXT,
    provider TEXT,
    attributes TEXT,  -- JSON blob of host attributes
    updated_at INTEGER,
    PRIMARY KEY (host, provider)
);

CREATE INDEX idx_provider_updated ON cache(provider, updated_at);
CREATE INDEX idx_host ON cache(host);
```

## Migration Strategy
- **Backward Compatibility**: Keep JSON cache as fallback during transition
- **Data Migration**: Script to import existing cache files into SQLite
- **Configuration**: New flag to choose cache backend

## Risks & Mitigation
- **SQLite Dependency**: Minimal, SQLite is widely supported
- **Concurrent Access**: SQLite WAL mode handles multiple processes
- **File Locking**: Better than current file-based approach
- **Cross-platform**: SQLite works on all herd-supported platforms

## Files Requiring Changes
- `provider/cache/provider.go` (new sqlcache variant)
- `registry.go` (minimal cache setup changes)
- Provider magic constructors (wrapper type change)

## Expected Impact
- **Zero API changes**: Same command-line interface and behavior
- **Performance improvement**: Faster queries, better concurrency
- **Operational improvement**: Single cache file vs many JSON files