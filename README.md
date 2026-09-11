# Beckn-ONIX

<div align="center">

[![CI](https://github.com/OpenAgriNet/network-adapter/actions/workflows/ci.yml/badge.svg?branch=release-0.0.1)](https://github.com/OpenAgriNet/network-adapter/actions/workflows/ci.yml)
[![Coverage](https://codecov.io/gh/OpenAgriNet/network-adapter/branch/release-0.0.1/graph/badge.svg)](https://codecov.io/gh/OpenAgriNet/network-adapter)
[![Security](https://github.com/OpenAgriNet/network-adapter/actions/workflows/security.yml/badge.svg?branch=release-0.0.1)](https://github.com/OpenAgriNet/network-adapter/security/code-scanning)
[![Go Version](https://img.shields.io/github/go-mod/go-version/OpenAgriNet/network-adapter)](https://golang.org)
[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)

**A production-ready, plugin-based middleware adapter for the Beckn Protocol**

[Overview](#overview) • [Features](#features) • [Architecture](#architecture) • [Key Aspects](#key-aspects) • [Quick Start](#quick-start) • [Documentation](#documentation) • [Contributing](#contributing)

</div>

---
## Latest Update
In August 2025, a completely new Beckn-ONIX adapter was made available. This version introduces a Plugin framework at it's core. 
The ONIX Adapter previous to this release is archived to a separate branch, [main-pre-plugins](https://github.com/beckn/beckn-onix/tree/main-pre-plugins) for reference.

## Overview

Beckn-ONIX is a middleware adapter that carries messages across a Beckn network. It sits between Beckn Application Platforms (BAPs — buyer applications) and Beckn Provider Platforms (BPPs — seller platforms), and it authenticates, validates and routes every message that passes through.

**This repository is OpenAgriNet's fork**, tracking [beckn/beckn-onix](https://github.com/beckn/beckn-onix). What it adds is the **provider adapter**: a `bpp`-role deployment that answers Beckn `select` requests synchronously for four agriculture capabilities — weather, mandi prices, knowledge advisory and agriculture facilities — by resolving each one's call plan from the registry, calling the upstream provider, and mapping the answer back. Everything below that is not marked as such is upstream's, and still true here.

### What is Beckn Protocol?

The **Beckn Protocol** is an open protocol that enables location-aware, local commerce across any platform and any domain. It allows creation of open, decentralized networks where:

- **Platform Independence**: Buyers and sellers can transact regardless of the platforms they use
- **Interoperability**: Seamless communication between different systems using standardized protocols
- **Domain Agnostic**: Works across retail, mobility, healthcare, logistics, and other domains
- **Network Neutral**: Can be deployed in any Beckn-compliant network globally

### Key Concepts

- **BAP (Beckn Application Platform)**: Buyer-side applications that help users search for and purchase products/services (e.g., consumer apps, aggregators)
- **BPP (Beckn Provider Platform)**: Seller-side platforms that provide products/services (e.g., merchant platforms, service providers)
- **Beckn Network**: Any network implementing the Beckn Protocol for enabling open commerce

## Features

### 🔌 **Plugin-Based Architecture**
- **Dynamic Plugin Loading**: Load and configure plugins at runtime without code changes
- **Extensible Design**: Easy to add new functionality through custom plugins
- **Hot-Swappable Components**: Update plugins without application restart (in development)

### 🔐 **Enterprise Security**
- **Ed25519 Digital Signatures**: Cryptographically secure message signing and validation
- **HashiCorp Vault Integration**: Centralized secrets and key management
- **Request Authentication**: Every message is authenticated and validated
- **TLS/SSL Support**: Encrypted communication channels

### ✅ **Protocol Compliance**
- **JSON Schema Validation**: Ensures all messages comply with Beckn protocol specifications
- **Version Management**: Support for multiple protocol versions simultaneously
- **Domain-Specific Schemas**: Tailored validation for different business domains

### 🚀 **High Performance**
- **Redis Caching**: Response caching for improved performance
- **RabbitMQ Integration**: Asynchronous message processing via message queues
- **Connection Pooling**: Efficient resource utilization
- **Configurable Timeouts**: Fine-tuned performance controls

### 📊 **Observability**
- **Structured Logging**: JSON-formatted logs with contextual information
- **Transaction Tracking**: End-to-end request tracing with unique IDs
- **OpenTelemetry Metrics**: Performance and business metrics collection
- **Runtime Instrumentation**: Go runtime + Redis client metrics included
- **Health Checks**: Liveness and readiness probes for Kubernetes

### 🌾 **Agriculture Capabilities**

The four capabilities this fork serves, each a step plugin that recognises its
own work by the binding key in the payload and passes the request through
untouched otherwise — so all four sit in one pipeline with no routing table:

| Capability | Provider | Serves |
|---|---|---|
| [`WeatherObservation`](pkg/plugin/implementation/WeatherObservation) | IMD Mausamgram | Daily forecast for a location |
| [`MandiPrice`](pkg/plugin/implementation/MandiPrice) | Agmarknet (Vistaar) | Commodity prices at a market |
| [`KnowledgeAdvisory`](pkg/plugin/implementation/KnowledgeAdvisory) | Bharat Vistaar | Retrieval-backed advisory passages |
| [`AgricultureFacility`](pkg/plugin/implementation/AgricultureFacility) | POCRA | The four governed facility types |

Adding a fifth is not a Go change here: a registry row binding
`<participantId>\|<capabilityCode>` to a call plan, one mapping file per action,
and one more entry under `providerSteps`. See
[`config/provider-adapter.yaml`](config/provider-adapter.yaml), which documents
each of these in place.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    HTTP Request                          │
└────────────────────────┬────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────┐
│                   Module Handler                         │
│  (bapTxnReceiver/Caller, bppTxnReceiver/Caller,         │
│   or oanProvider)                                        │
└────────────────────────┬────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────┐
│                 Processing Pipeline                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐    │
│  │ Middleware  │→ │   Steps     │→ │   Plugins   │    │
│  │(preprocess) │  │(validate,   │  │(cache,router│    │
│  └─────────────┘  │route, sign) │  │validator...)│    │
│                   └─────────────┘  └─────────────┘    │
└────────────────────────┬────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────┐
│              External Services/Response                  │
└─────────────────────────────────────────────────────────┘
```

### Core Components

#### 1. **Modules**
- `bapTxnReceiver`: Receives callback responses at BAP
- `bapTxnCaller`: Sends requests from BAP to BPP
- `bppTxnReceiver`: Receives requests at BPP
- `bppTxnCaller`: Sends responses from BPP to BAP
- `oanProvider`: Answers actions synchronously from a provider — this fork's module, mounted at `/`

#### 2. **Processing Steps**
- `validateSign`: Validates digital signatures on incoming requests
- `addRoute`: Determines routing based on configuration
- `validateSchema`: Validates against JSON schemas
- `sign`: Signs outgoing requests
- `signAck`: Signs whatever a provider step answered with
- `cache`: Caches requests/responses
- `publish`: Publishes messages to queue

A capability is a step too — the provider adapter's pipeline is
`validateSign → validateSchema → the four capability steps → signAck`. Each
capability step matches the binding key in the payload, serves the request if it
is one of its own, and returns the payload untouched if it is not, so dispatch
needs no routing table to keep in step with the registry.

#### 3. **Plugin Types**
- **Cache**: Redis-based response caching 
- **Router**: YAML-based routing rules engine for request forwarding (supports domain-agnostic routing for Beckn v2.x.x)
- **Registry**: Standard Beckn registry or Beckn One DeDi registry lookup for participant information
- **SunbirdRegistry**: SunbirdRC registry client, serving both halves of the lookup — the sender's signing key for `validateSign`, and the capability call plans the provider steps resolve against. See [plugin docs](pkg/plugin/implementation/sunbirdRegistry/README.md).
- **JsonMapper**: JSONata mapper (id: `jsonmapper`). Knows nothing about any provider: it fetches whatever URL the registry's `mappings` field names, compiles the JSONata, caches the compiled form and runs it in both directions. See [plugin docs](pkg/plugin/implementation/jsonmapper/README.md).
- **Signer**: Ed25519 digital signature creation for outgoing requests
- **SignValidator**: Ed25519 signature validation for incoming requests
- **SchemaValidator**: JSON schema validation
- **Schemav2Validator**: OpenAPI 3.x schema validation with action-based matching 
- **KeyManager**: HashiCorp Vault integration for production key management
- **SimpleKeyManager**: Embedded key management for local development (no external dependencies)
- **Publisher**: RabbitMQ message publishing for asynchronous processing
- **Encrypter**: AES encryption for sensitive data protection
- **Decrypter**: AES decryption for encrypted data processing
- **ReqPreprocessor**: Request preprocessing (UUID generation, headers)
- **ReqMapper**: Step plugin (id: `reqmapper`) used by the handler's `transformPayload` step to transform payloads at an explicit point in the pipeline.
- **OtelSetup**: Observability setup for metrics, traces, and logs (OTLP). Supports optional audit log configuration via `auditFieldsConfig` (YAML mapping actions to fields) . See [CONFIG.md](CONFIG.md) for details.
- **OpaPolicyChecker**: OPA-based network business policy enforcement. Evaluates Rego policies per request; supports network-specific policy configs, signed policy artifact verification, manifest-backed policies, and hot-reload. See [plugin docs](pkg/plugin/implementation/opapolicychecker/README.md).
- **ManifestLoader**: Fetches a network manifest published by a Network Facilitator Organization (NFO), verifies its detached signature, and caches the verified document for downstream consumers such as `opapolicychecker`. See [plugin docs](pkg/plugin/implementation/manifestloader/README.md).
- **PayloadStore**: Records every inbound request payload indexed by `message_id` and `transaction_id` with TTL-based expiration via the cache backend. Enables stateful use cases (duplicate detection, transaction history) without requiring other plugins to manage storage. See [plugin docs](pkg/plugin/implementation/payloadstore/README.md).
- **SchemaVersionMediator**: Allows network participants to evolve their domain schemas independently — without coordinating changes with the rest of the network. When a BAP and BPP declare different schema object versions in their node manifests, the mediator fetches JSONata translation artifacts from the network's artifact registry and patches the payload in-flight so each side receives data in the version it expects. This enables Beckn networks to evolve organically: a participant can adopt a new schema version the moment their own node is ready, without blocking or breaking their counterparties. See [plugin docs](pkg/plugin/implementation/schemaversionmediator/README.md).
- **VCValidator**: Step plugin (id: `validateVC`) that verifies W3C Verifiable Credentials embedded in request payloads for configured beckn actions — proof signature (did:key / did:jwk / did:web), issuer binding, validity window, and revocation (StatusList2021/Bitstring, DEDI) — rejecting failures with a signed NACK before routing. See [plugin docs](pkg/plugin/implementation/vcvalidator/README.md).

## Key Aspects

### 📊 Observability

Beckn-ONIX emits structured logs, OpenTelemetry metrics, and distributed traces via the `otelsetup` plugin. A reference collector stack (OpenTelemetry Collector, Grafana, Loki) is included in `install/network-observability/` for local and production use.

See [OBSERVABILITY.md](pkg/plugin/implementation/otelsetup/OBSERVABILITY.md) for the full architecture, signal catalogue, and setup guide.

### 🔒 Network Business Policy Enforcement

The `opapolicychecker` plugin evaluates [OPA](https://www.openpolicyagent.org/) Rego policies against every Beckn request. Policies can be scoped per network, distributed as signed artifacts by a Network Facilitator Organization (NFO), and hot-reloaded without a restart.

See [opapolicychecker README](pkg/plugin/implementation/opapolicychecker/README.md) for policy authoring, configuration, signature verification, and manifest-backed policy setup.

### ⚡ Performance

Beckn-ONIX is benchmarked end-to-end using Go's native `testing.B` framework — no Docker or external services required.

```bash
# Install benchstat (one-time)
go install golang.org/x/perf/cmd/benchstat@latest

# Run all benchmark scenarios and generate report
bash benchmarks/run_benchmarks.sh
```

Results land in `benchmarks/results/<timestamp>/`, which is generated and not
committed. The committed reports are in `benchmarks/reports/`; the latest is
[REPORT_ONIX_v172.md](benchmarks/reports/REPORT_ONIX_v172.md), with
[REPORT_ONIX_v150.md](benchmarks/reports/REPORT_ONIX_v150.md) kept for
comparison. See [benchmarks/README.md](benchmarks/README.md) for methodology and
interpretation guidance.

---

## Quick Start

### Prerequisites

- Go 1.26.8 or higher — the version `go.mod` requires, and the floor that
  clears the current Go standard-library advisories (`make security`)
- Redis (for caching)
- Docker (optional, for containerized deployment)

### Build and Run

1. **Clone the repository**
```bash
git clone https://github.com/OpenAgriNet/network-adapter.git
cd network-adapter
```

2. **Build the application**
```bash
go build -o server cmd/adapter/main.go
```

3. **Build plugins**
```bash
./install/build-plugins.sh
```

4. **Extract schemas**
```bash
unzip schemas.zip
```

5. **Start Redis** (if not running)
```bash
docker run -d -p 6379:6379 redis:alpine
```

6. **Update the config file**

**Note**: You can modify the configuration file to suit your environment before starting the server. ONIX adapter/server must be restarted to reflect any change made to the config file.

The following config change is required to all cache related entries in order to connect to `redis` that was started earlier.
```yaml
        cache:
          id: cache
          config:
            addr: localhost:6379
```

7. **Run the application**

```bash
./server --config=config/local-simple.yaml
```

The server will start on `http://localhost:8081`

### Running the provider adapter

`config/provider-adapter.yaml` is the deployment this fork ships, and it needs
two things `local-simple.yaml` does not. Fill in the `<>` placeholders — the
registry URL, the `subscriberId` (in **both** places), and each provider's
token endpoint — and export the credentials, which are named in the config but
never held by it:

```bash
export MAUSAMGRAM_AUTH='Basic <token>'            # WeatherObservation
export AGMARKNET_ACCESS_NAME=... AGMARKNET_PASSWORD=...   # MandiPrice
export VISTAAR_CLIENT_ID=... VISTAAR_CLIENT_SECRET=...    # KnowledgeAdvisory
                                                   # AgricultureFacility: none

./server --config=config/provider-adapter.yaml     # http://localhost:8080
```

A capability whose variables are unset loads cleanly, registers cleanly, passes
startup validation and then fails every one of its requests — the step refuses
to call an upstream unauthenticated rather than calling it without credentials.

### Automated Setup (Recommended)

For local setup, starts only redis and onix adapter:

```bash
# Clone and setup everything automatically
git clone https://github.com/OpenAgriNet/network-adapter.git
cd network-adapter/install
chmod +x setup.sh
./setup.sh
```

This automated script will:
- Start Redis container
- Build all plugins with correct Go version
- Build the adapter server
- Start ONIX adapter in Docker
- Create environment configuration

**Note:**
- **Schema Validation**: Extract schemas before running: `unzip schemas.zip` (required for `schemavalidator` plugin)
- **Alternative**: You can use `schemav2validator` plugin instead, which fetches schemas from a URL and doesn't require local schema extraction. See [CONFIG.md](CONFIG.md) for more configuration details.
- **Optional**: Before running the automated setup, build the adapter image and update `docker-compose-adapter.yaml` to use the correct image

```bash
# from the repository root
docker build -f Dockerfile.adapter-with-plugins -t beckn-onix:latest .
```

**For detailed setup instructions, see [SETUP.md](SETUP.md)**

**Services Started:**
- Redis: localhost:6379
- ONIX Adapter: http://localhost:8081

### Docker Deployment

**Note:** Start redis before before running onix adapter.

```bash
# Build the Docker image
 docker build -t beckn-onix:latest -f Dockerfile.adapter-with-plugins .

# Run the container
docker run -p 8081:8081 \
  -v $(pwd)/config:/app/config \
  -v $(pwd)/schemas:/app/schemas \
  -e CONFIG_FILE="/app/config/local-simple.yaml" \
  beckn-onix:latest
```

## Configuration

### Configuration Structure

A config names the modules to mount, the plugins each one loads, and the ordered
`steps` that run. Declaring a plugin is not enough — `steps` is what executes.
Abridged from [`config/local-simple.yaml`](config/local-simple.yaml):

```yaml
appName: "onix-local"
log:
  level: debug
  destinations:
    - type: stdout
http:
  port: 8081
  timeout:
    read: 30
    write: 30
    idle: 30
pluginManager:
  root: ./plugins
modules:
  - name: bapTxnReceiver
    path: /bap/receiver/
    handler:
      type: std
      role: bap
      plugins:
        cache:
          id: cache
          config:
            addr: localhost:6379
        router:
          id: router
          config:
            routingConfig: ./config/local-simple-routing.yaml
        schemaValidator:
          id: schemavalidator  # or schemav2validator
          config:
            schemaDir: ./schemas  # for schemavalidator
            # type: url           # for schemav2validator
            # location: https://example.com/spec.yaml
      steps:
        - validateSign
        - addRoute
        - validateSchema
```

### Deployment Modes

| Mode | Config | Notes |
|---|---|---|
| **Provider adapter** | [`config/provider-adapter.yaml`](config/provider-adapter.yaml) | What this fork deploys. Synchronous — see below. Port 8080. |
| Local development, combined | `config/local-simple.yaml` | BAP and BPP in one process, `simplekeymanager` with embedded Ed25519 keys, no Vault. Port 8081. |
| Local development, alternative | `config/local-dev.yaml` | As above but `keymanager`; needs a Vault. |
| Local with observability | `config/local-beckn-one-bap.yaml`, `config/local-beckn-one-bpp.yaml` | Add `otelsetup` (metrics, traces, audit logs) against an OTLP collector. Audit fields in `config/audit-fields.yaml`; full stack in `install/network-observability/`. |
| Combined / BAP-only / BPP-only | `config/onix/`, `config/onix-bap/`, `config/onix-bpp/` | Upstream production layouts, `secretskeymanager` (HashiCorp Vault). |

Placeholders written `<>` in `provider-adapter.yaml` are deliberate — a registry
URL, a `subscriberId` and a provider token endpoint are per-deployment, and a
working value copied from a README signs as somebody else.

## API Endpoints

There is no fixed endpoint list. A module mounts a **subtree**, its mount path
is stripped off the request path, and whatever remains — `select`, `discover` —
is the action the routing config matches on. So which actions exist is a
property of the routing config and the registry, not of the adapter.

### Provider adapter (`config/provider-adapter.yaml`)

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/{action}` | The `oanProvider` module, mounted at `/`. `select` is the action the four capabilities serve today. |

It answers **synchronously**: verify the sender, resolve the capability's call
plan from the registry, call the provider, map the result, sign and return it.
There is no callback — the answer is the HTTP response. A payload no capability
claims gets `404 NET_ENTITY_NOT_FOUND`, deliberately not an ACK, which would
leave the caller waiting for a callback nobody will send.

### Transaction modules (`config/local-simple.yaml`)

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/bap/caller/{action}` | Sends requests from BAP to BPP |
| POST | `/bap/receiver/{action}` | Receives callbacks at BAP |
| POST | `/bpp/receiver/{action}` | Receives requests at BPP |
| POST | `/bpp/caller/{action}` | Sends callbacks from BPP to BAP |

## Documentation

- **[Setup Guide](SETUP.md)**: Complete installation, configuration, and deployment instructions
- **[Configuration Guide](CONFIG.md)**: Description of Configuration concepts and all config parameters 
- **[Contributing](CONTRIBUTING.md)**: Guidelines for contributors
- **[Governance](GOVERNANCE.md)**: Project governance model
- **[License](LICENSE)**: Apache 2.0 license details

## Testing

Use the `make` targets rather than a bare `go test ./...`. The plugin packages
have to be carved out of the race-instrumented build: `pkg/plugin` and
`benchmarks/e2e` compile a real `.so` with `go build -buildmode=plugin` and
then `plugin.Open` it in the same run, and a race-instrumented test binary
cannot load a non-race `.so`. `make test` splits the invocation accordingly;
`go test -race ./...` does not, and fails in a way that looks like a bug in the
plugin loader.

```bash
make test        # both suites, race detection where it is safe
make cover       # the same, writing a merged profile to coverage.out
make cover-diff  # coverage of the files this branch changed vs BASE_REF
make lint        # golangci-lint run + fmt --diff
make build       # the adapter binary, into bin/

# A single package is still a plain go test
go test ./pkg/plugin/implementation/cache -v
```

`make cover-diff` is the coverage gate, and it is scoped to the diff on
purpose: the repo's whole-tree total is below `MIN_COVERAGE`, so a whole-repo
gate would fail every PR over a backlog none of them created. What a review can
act on is the number for the lines the PR itself touched.

## Security scanning

Three scanners, because they are blind to different things:

```bash
make trivy-deps   # the module graph — catches a vulnerable module only the
                  #   tests import, which never reaches a layer
make docker && make trivy-image
                  # the image — base layers plus the Go build info compiled
                  #   into the binary, so a toolchain CVE shows up here
make trivy-gate   # fails if either SARIF report carries a finding
make security     # govulncheck: the call graph, so it reports a CVE only
                  #   when the vulnerable symbol is actually reachable
```

`make security` is also the only one that judges the toolchain `go.mod`
requires rather than the one the Dockerfile pins — which is why the
prerequisite above names a specific patch version.

## CI

| Workflow | Runs on | What it answers |
|---|---|---|
| [`ci.yml`](.github/workflows/ci.yml) | every PR, and pushes to the trunk | Does this diff build, test and scan clean? Posts the coverage and Trivy comments. |
| [`codeql.yml`](.github/workflows/codeql.yml) | every PR, trunk, weekly | Does the code in this repo contain a vulnerability — injection, request forgery, key material reaching a log? Reports to the Security tab; does not block. |
| [`coverage.yml`](.github/workflows/coverage.yml) | trunk only | What is the whole-repo total, for the badge. |
| [`security.yml`](.github/workflows/security.yml) | trunk, weekly | Is anything wrong with the trunk *today* — including a CVE published against code nobody has touched since? |
| [`ci-release.yml`](.github/workflows/ci-release.yml) | version tags | Build, rescan and publish the images a release ships. |

The weekly schedules are the point of the last two: a PR scan can only tell you
whether a diff introduced something, and almost every real finding arrives
against code that has not changed.

Every CI step is a one-line `make` call, so a red check reproduces locally by
running the command its log shows. Thresholds and tool versions live in the
`Makefile`, never duplicated into a workflow `env:` block.

The two badges at the top read `release-0.0.1` — the branch this service
actually ships from — and they are measured after a merge, not on a pull
request. The per-PR gates are the first two rows above.

## Contributing

We welcome contributions! Please see our [Contributing Guide](CONTRIBUTING.md) for details on:
- Code of Conduct
- Development process
- Submitting pull requests
- Reporting issues

## Support

- **Issues**: [GitHub Issues](https://github.com/OpenAgriNet/network-adapter/issues)
- **Discussions**: [GitHub Discussions](https://github.com/OpenAgriNet/network-adapter/discussions)
- **Security advisories**: [Code scanning alerts](https://github.com/OpenAgriNet/network-adapter/security/code-scanning) — see also [SECURITY.md](SECURITY.md)

Upstream — the protocol adapter this fork tracks — is [beckn/beckn-onix](https://github.com/beckn/beckn-onix); issues about the Beckn adapter itself, rather than about this OpenAgriNet deployment of it, belong there.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- [Beckn Foundation](https://beckn.org) for the protocol specifications

| Contributor | Organization | Github ID |
|--------|----------|-------------|
| Ashish Guliya | Google Cloud | ashishkgGoogle |
| Pooja Joshi | Google Cloud | poojajoshi2 |
| Deepa Mulchandani | Google Cloud | Deepa-Mulchandani |
| Ankit | Beckn Labs | ankitShogun |
| Abhishek | Beckn Labs | em-abee |
| Viraj Kulkarni | Beckn Labs | viraj89 |
| Amay Pandey | Google Cloud |  |
| Dipika Prasad | Google Cloud | DipikaPrasad |
| Tanya Madaan | ONDC | tanyamadaan |
| Binu | ONDC |  |
| Faiz M | Beckn Labs | faizmagic |
| Ravi Prakash | Beckn Labs | ravi-prakash-v |
| Siddharth Prakash | Google Cloud |  |
| Namya Patiyal | Google Cloud |  |
| Saksham Nagpal | Google Cloud | sakshamGoogle |
| Arpit Bharadwaj | Google Cloud |  |
| Pranoy | Google Cloud |  |
| Mayuresh Nirhali | Beckn Labs | nirmay |
| Madhuvandhini B | Google Cloud | madhuvandhini5856 |
| Siddhartha Banerjee | Google Cloud | sidb85 |
| Manendra Pal Singh | NPCI BHIM | manendrapalsingh |

---

<div align="center">
Built with ❤️ for the open Value Network ecosystem
</div>
