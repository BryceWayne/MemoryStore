# Security Policy

## Supported Versions

The following versions of **MemoryStore** are currently supported with security updates:

| Version | Go Version | Supported |
| :--- | :--- | :---: |
| `1.x` / `root` | Go 1.27+ | :white_check_mark: |
| `< 1.0.0` | Go < 1.27 / pre-1.0 | :x: |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

Instead, please report them privately using GitHub Security Advisories:

- **GitHub Security Advisory:** [Report a vulnerability](https://github.com/BryceWayne/MemoryStore/security/advisories/new)

Please include as much detail as possible to help us reproduce and assess the issue quickly:

- A clear description of the vulnerability and its potential impact
- Steps to reproduce or a minimal Go proof-of-concept
- Affected MemoryStore version / commit SHA and Go version
- Any suggested fix or mitigation, if known

## Response Timeline

- **Acknowledgement:** Within 3 business days of receipt
- **Triage & Assessment:** Within 7 business days
- **Fix & Coordinated Disclosure:** We will work with you to verify a patch and coordinate a release and advisory once a fix is available.

## Security Design Practices

- Core JSON serialization uses Go 1.27's memory-safe standard library `encoding/json/v2`, which rejects duplicate JSON object keys and invalid UTF-8 by default.
- Concurrency paths are continuously validated with Go's `-race` detector and `goroutineleak` profiling.
- Direct third-party dependencies are kept minimal and tracked via Dependabot.
