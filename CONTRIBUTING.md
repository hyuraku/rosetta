# Contributing to Rosetta

Thank you for your interest in contributing to Rosetta! This document provides guidelines and instructions for contributing to the project.

## Table of Contents
- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [How to Contribute](#how-to-contribute)
- [Code Style](#code-style)
- [Testing](#testing)
- [Pull Request Process](#pull-request-process)
- [Reporting Bugs](#reporting-bugs)
- [Suggesting Features](#suggesting-features)

## Finding Work

Looking for something to work on? Check out:
- **[TODO.md](TODO.md)** - Planned features and enhancements
- **GitHub Issues** - Open issues and feature requests
- **Documentation** - Areas needing improvement or clarification

Features are prioritized as:
- 🔴 High Priority - Critical for production
- 🟡 Medium Priority - Important functionality
- 🟢 Low Priority - Nice to have enhancements

## Code of Conduct

### Our Standards

- Be respectful and inclusive
- Welcome newcomers and help them learn
- Focus on what is best for the community
- Show empathy towards other community members
- Provide constructive feedback

### Unacceptable Behavior

- Harassment, discrimination, or offensive comments
- Trolling, insulting, or derogatory remarks
- Publishing private information without permission
- Other conduct that would be inappropriate in a professional setting

## Getting Started

### Prerequisites

- Go 1.23 or later
- Git
- Make (for using Makefile commands)
- Basic understanding of distributed systems and Raft consensus
- Familiarity with Go programming

### Fork and Clone

1. Fork the repository on GitHub
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR-USERNAME/rosetta.git
   cd rosetta
   ```

3. Add upstream remote:
   ```bash
   git remote add upstream https://github.com/ORIGINAL-OWNER/rosetta.git
   ```

## Development Setup

### 1. Install Dependencies

```bash
go mod download
```

### 2. Build the Project

```bash
# Using Makefile (recommended)
make build

# Or manually
go build -ldflags="-linkmode=external" -o rosetta main.go
```

**Note for macOS Users**: The `-ldflags="-linkmode=external"` flag is required to avoid dyld UUID warnings on macOS Sequoia and later. The Makefile handles this automatically.

### 3. Run Tests

```bash
# Using Makefile (recommended)
make test              # Run unit and integration tests
make test-unit         # Run unit tests only
make test-integration  # Run integration tests only
make test-race         # Run tests with race detector
make bench             # Run benchmarks

# Or manually with ldflags
go test ./... -v -ldflags="-linkmode=external"
go test ./... -race -ldflags="-linkmode=external"
```

### 4. Run Local Cluster

```bash
cd examples/simple-cluster
./start.sh
```

### Development Tools

**Recommended Tools:**
- `gofmt` - Code formatting
- `go vet` - Static analysis
- `golint` - Linting
- `gopls` - Language server for IDE integration

**Install Tools:**
```bash
go install golang.org/x/tools/cmd/goimports@latest
go install golang.org/x/lint/golint@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
```

## How to Contribute

### Types of Contributions

We welcome many types of contributions:

- **Bug Fixes**: Fix issues and improve stability
- **Features**: Add new functionality
- **Documentation**: Improve docs, add examples, fix typos
- **Tests**: Add test coverage, improve test quality
- **Performance**: Optimize code, reduce latency
- **Refactoring**: Improve code quality and maintainability

### Finding Issues to Work On

- Check the [Issues](https://github.com/OWNER/rosetta/issues) page
- Look for issues labeled `good first issue` or `help wanted`
- Ask in the issue comments if you'd like to work on something

### Before You Start

1. **Check existing issues** - Someone may already be working on it
2. **Open an issue** - For significant changes, discuss your approach first
3. **Get consensus** - Ensure the change aligns with project goals
4. **Assign yourself** - Comment on the issue to let others know you're working on it

## Code Style

### Go Conventions

Follow standard Go conventions:

```bash
# Format code
gofmt -w .

# Organize imports
goimports -w .

# Run linter
golint ./...

# Static analysis
go vet ./...
```

### Naming Conventions

- **Packages**: lowercase, single word (`raft`, `kvstore`)
- **Types**: PascalCase (`RaftNode`, `KVStore`)
- **Functions**: PascalCase for exported, camelCase for private
- **Variables**: camelCase
- **Constants**: PascalCase (`Leader`, `Follower`)

### Code Organization

```go
// Package comment
package example

// Imports grouped: stdlib, external, internal
import (
    "fmt"
    "time"

    "github.com/external/pkg"

    "github.com/rosetta/raft"
)

// Constants
const (
    DefaultTimeout = 5 * time.Second
)

// Types
type Example struct {
    field string
}

// Constructor
func NewExample() *Example {
    return &Example{}
}

// Methods
func (e *Example) Method() {
    // implementation
}
```

### Documentation

- Document all exported types, functions, and methods
- Use complete sentences
- Start with the name of the thing being documented

```go
// RaftNode represents a node in the Raft cluster.
// It manages state transitions, log replication, and leader election.
type RaftNode struct {
    // ...
}

// Start begins the Raft node's operation.
// It initializes the event loop and starts processing events.
func (n *RaftNode) Start() error {
    // ...
}
```

### Error Handling

```go
// Return errors explicitly
func DoSomething() error {
    if err := validate(); err != nil {
        return fmt.Errorf("validation failed: %w", err)
    }
    return nil
}

// Use errors.Is and errors.As for error checking
if errors.Is(err, ErrNotFound) {
    // handle not found
}
```

## Testing

### Writing Tests

- Place tests in `*_test.go` files
- Name test functions `TestXxx`
- Use table-driven tests where appropriate

```go
func TestRaftStateTransition(t *testing.T) {
    tests := []struct {
        name      string
        fromState State
        toState   State
        valid     bool
    }{
        {"follower to candidate", Follower, Candidate, true},
        {"candidate to leader", Candidate, Leader, true},
        {"leader to follower", Leader, Follower, false},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // test implementation
        })
    }
}
```

### Test Coverage

- Aim for >80% code coverage
- Focus on critical paths
- Test edge cases and error conditions

```bash
# Check coverage
go test ./... -cover

# Generate coverage report
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

### Integration Tests

Integration tests go in `tests/integration/`:

```go
func TestClusterLeaderElection(t *testing.T) {
    // Create cluster
    cluster := NewTestCluster(3)
    defer cluster.Shutdown()

    // Wait for leader election
    cluster.WaitForLeader(time.Second)

    // Verify leader exists
    leader := cluster.GetLeader()
    if leader == nil {
        t.Fatal("no leader elected")
    }
}
```

### Running Tests

```bash
# Unit tests only
go test ./tests/unit/... -v

# Integration tests only
go test ./tests/integration/... -v

# All tests
go test ./... -v

# With race detection
go test ./... -race

# Specific test
go test ./raft -run TestRaftNode -v
```

## Pull Request Process

### 1. Create a Branch

```bash
# Update main branch
git checkout main
git pull upstream main

# Create feature branch
git checkout -b feature/your-feature-name
```

### 2. Make Changes

- Follow code style guidelines
- Write tests for new functionality
- Update documentation as needed
- Keep commits focused and atomic

### 3. Commit Changes

Write clear commit messages:

```bash
git add .
git commit -m "feat: add leader election timeout configuration

- Add configurable election timeout
- Update tests to cover new behavior
- Document new configuration option

Fixes #123"
```

**Commit Message Format:**
```
<type>: <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `test`: Adding or updating tests
- `refactor`: Code refactoring
- `perf`: Performance improvements
- `chore`: Maintenance tasks

### 4. Push and Create PR

```bash
# Push to your fork
git push origin feature/your-feature-name

# Create PR on GitHub
```

### 5. PR Checklist

Before submitting, ensure:

- [ ] Code follows project style guidelines
- [ ] All tests pass (`go test ./...`)
- [ ] New tests added for new functionality
- [ ] Documentation updated (if applicable)
- [ ] No linting errors (`golint ./...`)
- [ ] No race conditions (`go test ./... -race`)
- [ ] Commit messages are clear
- [ ] PR description explains the changes

### 6. Code Review

- Be responsive to feedback
- Make requested changes promptly
- Discuss disagreements constructively
- Update your PR based on reviews

```bash
# Make changes based on review
git add .
git commit -m "address review feedback"
git push origin feature/your-feature-name
```

### 7. Merge

Once approved:
- Maintainer will merge your PR
- Your contribution will be included in the next release

## Reporting Bugs

### Before Reporting

1. **Search existing issues** - Your bug may already be reported
2. **Try latest version** - Bug may be fixed in newer version
3. **Isolate the problem** - Create minimal reproduction case

### Bug Report Template

```markdown
**Description**
Clear description of the bug

**To Reproduce**
Steps to reproduce:
1. Start cluster with '...'
2. Execute command '...'
3. See error

**Expected Behavior**
What you expected to happen

**Actual Behavior**
What actually happened

**Environment**
- OS: [e.g., macOS 13.0]
- Go Version: [e.g., 1.20]
- Rosetta Version: [e.g., v0.1.0]
- Cluster Size: [e.g., 3 nodes]

**Logs**
```
paste relevant logs here
```

**Additional Context**
Any other relevant information
```

## Suggesting Features

### Feature Request Template

```markdown
**Is your feature request related to a problem?**
Clear description of the problem

**Describe the solution you'd like**
Clear description of desired solution

**Describe alternatives considered**
Other solutions you've considered

**Additional context**
- Use cases
- Benefits
- Potential drawbacks
- Implementation ideas
```

### Design Documents

For significant features:

1. Create a design document
2. Discuss in GitHub issue
3. Get consensus from maintainers
4. Implement after approval

## Development Workflow

### Typical Workflow

1. Pick an issue or create one
2. Discuss approach in issue comments
3. Fork and create branch
4. Implement changes with tests
5. Run full test suite
6. Update documentation
7. Create pull request
8. Address review feedback
9. Get approval and merge

### Best Practices

- **Keep PRs focused** - One feature/fix per PR
- **Write tests first** - TDD approach when possible
- **Small commits** - Easier to review and revert
- **Update docs** - Keep documentation in sync
- **Be patient** - Reviews take time

## Getting Help

### Resources

- **Documentation**: [docs/](docs/)
- **Examples**: [examples/](examples/)
- **Issues**: [GitHub Issues](https://github.com/OWNER/rosetta/issues)

### Questions

- Open an issue with the `question` label
- Provide context and what you've already tried
- Be specific about what you need help with

## Recognition

Contributors will be recognized:

- Listed in [CONTRIBUTORS.md](CONTRIBUTORS.md)
- Mentioned in release notes
- GitHub contributor badge

## License

By contributing, you agree that your contributions will be licensed under the same license as the project (MIT License).

---

Thank you for contributing to Rosetta! Your efforts help make distributed systems more accessible and reliable.
