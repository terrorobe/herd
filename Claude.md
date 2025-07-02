# Herd - Lightning-fast Parallel SSH Client

## Overview
Herd is a massively parallel SSH client written in Go that replaces traditional for loops, pssh, xargs, and GNU parallel approaches with a faster, more flexible solution. It discovers hosts from various sources (cloud providers, service discovery systems, files) and executes commands across them in parallel.

## Key Features
- **Parallel execution**: Run commands on thousands of hosts simultaneously
- **Multiple host discovery sources**: AWS, Azure, GCP, Consul, Prometheus, Tailscale, and more
- **Powerful query syntax**: Select hosts using glob patterns and attribute filters
- **Flexible output modes**: Stream output as it happens or show at the end
- **History tracking**: All commands and outputs are stored for later analysis
- **Interactive shell**: Create dynamic host lists based on command results
- **Plugin system**: Extend with custom host providers

## Architecture

### Core Components

1. **Main Entry Point** (`cmd/herd/main.go`)
   - CLI interface using Cobra
   - Configuration management with Viper
   - Signal handling for graceful shutdowns
   - History management and migration

2. **Registry System** (`registry.go`)
   - Central manager for all host providers
   - Coordinates concurrent host loading
   - Manages provider lifecycle and caching
   - Implements glob prefix filters (e.g., `file:hosts.txt`)

3. **Host and HostSet** (`host.go`, `hostset.go`)
   - Core data structures for representing hosts
   - Attribute system for flexible metadata
   - Filtering and search capabilities
   - Result tracking from command execution

4. **Runner** (`runner.go`)
   - Executes commands across host sets
   - Manages parallelism and timeouts
   - Implements splay (random delays) for rolling operations
   - Handles cancellation and signal propagation

5. **SSH Package** (`ssh/`)
   - SSH connection management
   - Agent pooling for performance
   - Host key scanning and verification
   - Support for various SSH configurations

### Provider System

The provider system is highly extensible through a well-defined interface:

```go
type HostProvider interface {
    Name() string
    Prefix() string
    ParseViper(v *viper.Viper) error
    Load(ctx context.Context, l LoadingMessage) (*HostSet, error)
    Equivalent(p HostProvider) bool
}
```

**Provider Categories:**
- **Cloud Providers**: AWS, Azure, Google Cloud, TransIP
- **Service Discovery**: Consul, Prometheus
- **File-based**: Plain text, JSON, SSH known_hosts
- **Network**: HTTP endpoints, Tailscale
- **Special**: Cache wrapper, Plugin loader, Example template

**Key Design Patterns:**
- Scatter-gather for concurrent operations
- Provider registration at startup
- Magic providers for auto-discovery
- Caching layer for fault tolerance
- Plugin system using gRPC

### Scripting Engine

Herd includes a custom scripting language (`scripting/`) with:
- ANTLR4-based parser
- Interactive command interpreter
- Built-in commands for host manipulation
- Integration with the main execution engine

### Output Formatting

Multiple output modes (`formatter.go`):
- **All**: Show all output at the end
- **Inline**: Mix output from different hosts
- **Per-host**: Group output by host
- **Tail**: Stream output with timestamps

## Command Examples

```bash
# List hosts in a domain
herd list *.example.com

# Run command on all hosts
herd run *.example.com -- uptime

# Long-running command with reduced parallelism
herd run *.example.com --parallel 10 --host-timeout 5m -- sudo puppet agent -t

# Query hosts by attributes
herd list site=site-5 --attributes ip_address,os,memory

# Rolling restart with delays
herd run --splay 1m --parallel 1 consul_service=smtpd -- sudo systemctl restart postfix

# Interactive mode
herd interactive *vpn-gateway*
```

## Building and Installation

The project uses Go modules and can be built with:
```bash
make
```

Pre-built binaries are available on the project website.

## Testing

The project includes:
- Unit tests throughout the codebase
- Integration tests using the Sharness framework (`integration/`)
- Test infrastructure with Docker containers
- Mock providers for testing

## Dependencies

Key dependencies include:
- Cobra/Viper for CLI framework
- Various cloud SDKs (AWS, Azure, GCP)
- HashiCorp libraries (Consul API, go-plugin)
- ANTLR4 for the scripting language
- SSH libraries for connection management

## Security Considerations

- SSH agent support with connection pooling
- Host key verification
- Plugin checksum verification
- Secure credential handling for cloud providers
- No hardcoded secrets in the codebase

## Development Notes

- Well-structured with clear separation of concerns
- Extensive use of Go interfaces for extensibility
- Concurrent operations using scatter-gather pattern
- Comprehensive error handling with multi-error support
- Cross-platform support (Unix/Windows)

## Development Memories

- I need a cross compile for a modern linux system for the herd cache bench tool
- Only amd64 needed
- Build the bench tool binary with a consistent naming convention to ensure reproducibility