# Documentation Index

Welcome to the Rosetta documentation! This guide will help you navigate all available documentation resources.

## 📚 Documentation Structure

### For New Users

Start here to get up and running quickly:

1. **[Project README](../README.md)** - Project overview and quick start
2. **[Simple Cluster Example](../examples/simple-cluster/)** - Hands-on tutorial
3. **[API Documentation](api.md)** - Complete API reference

### For Developers

Resources for contributing to the project:

1. **[Contributing Guide](../CONTRIBUTING.md)** - How to contribute
2. **[CLAUDE.md](../CLAUDE.md)** - AI assistant guidance
3. **[Code Style Guide](../CONTRIBUTING.md#code-style)** - Coding standards

### For Operations

Production deployment and maintenance:

1. **[Deployment Guide](deployment.md)** - Production setup
2. **[Troubleshooting](troubleshooting.md)** - Problem solving
3. **[Performance Tuning](performance.md)** - Optimization

## 📖 Core Documentation

### [API Documentation](api.md)
Complete HTTP API reference with request/response examples, error codes, and client implementation patterns.

**Contents:**
- Endpoint specifications (PUT, GET, DELETE, status, leader)
- Request/response formats
- Error handling
- Client implementation examples
- Security considerations

**Best for:** Application developers integrating with Rosetta

---

### [Deployment Guide](deployment.md)
Comprehensive guide for deploying Rosetta in production environments.

**Contents:**
- Cluster sizing and planning
- Deployment architectures
- Production setup (systemd, security)
- Monitoring and logging
- Backup and recovery
- Rolling upgrades

**Best for:** DevOps engineers, SREs

---

### [Troubleshooting Guide](troubleshooting.md)
Solutions for common issues and debugging techniques.

**Contents:**
- Quick diagnostics
- Common issues (startup, leader election, replication)
- Performance problems
- Network issues
- Debug tools and techniques

**Best for:** Operations teams, developers debugging issues

---

### [Performance Tuning Guide](performance.md)
Strategies for optimizing Rosetta performance.

**Contents:**
- Performance characteristics
- Benchmarking tools and methods
- Write/read optimization
- Network and resource tuning
- Scaling strategies

**Best for:** Performance engineers, architects

---

## 🚀 Examples & Tutorials

### [Simple Cluster Example](../examples/simple-cluster/)
Step-by-step guide to running a local 3-node cluster.

**What you'll learn:**
- Cluster setup and configuration
- Leader election in action
- Data replication
- Fault tolerance testing
- Recovery procedures

**Includes:**
- `start.sh` - Automated cluster startup
- `stop.sh` - Clean shutdown
- `demo.sh` - Interactive demonstration

---

### [Benchmark Example](../examples/benchmark/)
Performance testing tools and methodologies.

**What you'll learn:**
- Write and read performance testing
- Latency measurement
- Throughput analysis
- Load testing strategies

**Includes:**
- Custom benchmark tool (`benchmark.go`)
- Apache Bench examples
- wrk configurations
- Results interpretation

---

## 🤝 Contributing

### [Contributing Guide](../CONTRIBUTING.md)
Everything you need to contribute to Rosetta.

**Contents:**
- Development setup
- Code style and conventions
- Testing requirements
- Pull request process
- Bug reporting
- Feature requests

---

### [CLAUDE.md](../CLAUDE.md)
Special guidance for AI assistants working with this codebase.

**Contents:**
- Project overview
- Architecture details
- Development commands
- Implementation details
- Testing strategy

---

## 🗂️ Documentation by Topic

### Getting Started
- [README](../README.md) - Quick start
- [Simple Cluster](../examples/simple-cluster/) - Tutorial
- [API Docs](api.md) - API reference

### Development
- [Contributing](../CONTRIBUTING.md) - How to contribute
- [Code Style](../CONTRIBUTING.md#code-style) - Coding standards
- [Testing](../CONTRIBUTING.md#testing) - Test guidelines

### Operations
- [Deployment](deployment.md) - Production deployment
- [Monitoring](deployment.md#monitoring) - Health monitoring
- [Backup](deployment.md#backup-and-recovery) - Data backup

### Performance
- [Tuning](performance.md) - Optimization
- [Benchmarking](../examples/benchmark/) - Performance testing
- [Scaling](performance.md#scaling-strategies) - Scale strategies

### Troubleshooting
- [Common Issues](troubleshooting.md#common-issues) - FAQ
- [Debug Tools](troubleshooting.md#debug-tools) - Debugging
- [Network Issues](troubleshooting.md#network-issues) - Connectivity

### API & Integration
- [HTTP API](api.md) - REST API
- [Client Examples](api.md#client-implementation-pattern) - Client code
- [Error Handling](api.md#error-handling) - Error codes

## 🔍 Quick Reference

### Command Cheat Sheet

```bash
# Build
go build -o rosetta main.go

# Run node
./rosetta -id=node1 -listen=:8080 -http=:9080 -peers=node2:addr:port

# Test
go test ./... -v
go test ./... -race

# Benchmark
cd examples/benchmark && ./benchmark -url=http://localhost:9080

# Start example cluster
cd examples/simple-cluster && ./start.sh
```

### API Quick Reference

```bash
# Store data
curl -X PUT http://localhost:9080/kv -d '{"key":"foo","value":"bar"}'

# Retrieve data
curl http://localhost:9080/kv/foo

# Delete data
curl -X DELETE http://localhost:9080/kv/foo

# Node status
curl http://localhost:9080/status

# Leader info
curl http://localhost:9080/leader
```

### Configuration Quick Reference

| Flag | Description | Example |
|------|-------------|---------|
| `-id` | Node identifier | `node1` |
| `-listen` | Raft RPC address | `localhost:8080` |
| `-http` | HTTP API address | `localhost:9080` |
| `-peers` | Peer list | `node2:addr:port,node3:addr:port` |

## 📝 Documentation Status

| Document | Status | Last Updated |
|----------|--------|--------------|
| README | ✅ Complete | Current |
| API Docs | ✅ Complete | Current |
| Deployment | ✅ Complete | Current |
| Troubleshooting | ✅ Complete | Current |
| Performance | ✅ Complete | Current |
| Contributing | ✅ Complete | Current |
| Examples | ✅ Complete | Current |

## 🔗 External Resources

### Raft Consensus
- [Raft Paper](https://raft.github.io/raft.pdf) - Original Raft algorithm paper
- [Raft Visualization](http://thesecretlivesofdata.com/raft/) - Interactive visualization
- [Raft Website](https://raft.github.io/) - Official Raft website

### Go Resources
- [Go Documentation](https://go.dev/doc/) - Official Go docs
- [Effective Go](https://go.dev/doc/effective_go) - Go best practices
- [Go by Example](https://gobyexample.com/) - Go code examples

## 📧 Getting Help

If you can't find what you're looking for:

1. Check the [Troubleshooting Guide](troubleshooting.md)
2. Search [GitHub Issues](https://github.com/OWNER/rosetta/issues)
3. Open a new issue with the `question` label
4. Review [Contributing Guide](../CONTRIBUTING.md) for development questions

## 🎯 Documentation Goals

Our documentation aims to be:
- **Comprehensive** - Cover all aspects of the system
- **Accessible** - Easy to understand for all skill levels
- **Practical** - Include real-world examples
- **Up-to-date** - Reflect current implementation
- **Searchable** - Easy to find information

---

**Happy learning!** 🚀

For the most up-to-date information, always refer to the main [README](../README.md) and check the project's [GitHub repository](https://github.com/OWNER/rosetta).
