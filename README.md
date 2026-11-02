# CIC: Code-In-Container

A sandboxed code runner in Go. Send it source code in Python, Node.js, C, Rust, or Go, and it runs the code in a fresh, locked-down Docker container and returns the output, exit status, and timings. A warm pool of pre-started containers per language removes container startup from the request path.

Every run gets its own container, with no network, a read-only filesystem, a non-root user, no capabilities, and memory, process, CPU, and time limits. The container is destroyed afterwards. See [SECURITY.md](SECURITY.md) for the threat model, how it's tested, and the known gaps.

## Quick start

Needs Linux with Docker (cgroup v2) and Go 1.27 (older Go toolchains download it automatically).

```sh
./scripts/images.sh        # pull the language images and build cic-go (one time, a few GB)
go run ./cmd/cic           # listens on 127.0.0.1:8088
```

```sh
curl -s localhost:8088/v1/execute -d '{"language": "python", "code": "print(sum(range(10)))"}'
```

```json
{"status":"ok","stdout":"45\n","stderr":"","exit_code":0,"stdout_truncated":false,"stderr_truncated":false,
 "timings":{"queue_ms":0,"compile_ms":0,"run_ms":52,"total_ms":131},"warm":true}
```

Or with Docker Compose, which runs CIC behind a filtering Docker socket proxy so the service itself never holds the real socket (no Go needed after `images.sh`):

```sh
./scripts/images.sh
docker compose up -d       # also on 127.0.0.1:8088; `docker compose down` removes every sandbox
```

Either way, open http://127.0.0.1:8088 for a minimal test page: pick a language, edit the code and stdin, and see the status, output, and timings.

The API has no authentication. Keep it on localhost (see [SECURITY.md](SECURITY.md#known-gaps)).

## API

### `POST /v1/execute`

| Field | | |
|---|---|---|
| `language` | required | `python`, `node`, `c`, `rust`, or `go` |
| `code` | required | at most 64 KB |
| `stdin` | optional | at most 64 KB |
| `limits.timeout_ms` | optional | run timeout, default 5000, at most 15000 (compiling has its own 10 s) |
| `limits.memory_mb` | optional | run memory, 32 to 512, default 256 |

The response has `status`, `phase` (`compile` or `run`, when the status isn't `ok`), `stdout`, `stderr`, `exit_code`, truncation flags, `timings` (`queue_ms`, `compile_ms`, `run_ms`, `total_ms`), and `warm` (whether a pre-started container was used).

| `status` | Meaning |
|---|---|
| `ok` | exited 0 |
| `compile_error` | the compiler failed; its messages are in `stderr` |
| `runtime_error` | non-zero exit, a crash, or output past 256 KB (the program is killed) |
| `timeout` | killed at the time limit; output printed before that is kept |
| `memory_exceeded` | killed by the memory limit, during `compile` or `run` |
| `internal_error` | CIC itself failed (HTTP 500) |

When all `-max-runs` slots stay busy for 10 s, the request gets HTTP 503 with `Retry-After` instead.

Each of `stdout` and `stderr` keeps the first 64 KB. Bad requests get HTTP 400, and oversized ones get 413.

### `GET /v1/languages`

The languages that passed the startup check.

### `GET /v1/health`

Per language: idle containers in the pool, runs in progress, and requests waiting for a container.

```json
{"python":{"idle":2,"busy":0,"waiting":0}, ...}
```

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `127.0.0.1:8088` | listen address |
| `-config` | `config/languages.yaml` | language definitions |
| `-pool-size` | `2` | idle containers kept per language |
| `-queue-timeout` | `1s` | how long a request waits for a warm container before cold-starting one |
| `-max-runs` | CPU count | executions at once across all languages; extra requests wait up to 10 s, then get 503 |

Languages live in [`config/languages.yaml`](config/languages.yaml): image, user, compile and run commands, environment, and memory, process, and tmpfs budgets. Adding a language needs no code changes. A language whose image is missing, or lacks its user, is disabled at startup.

## How a run works

1. Take an idle container from the language's pool, or start one if none frees up within the queue timeout. Pooled containers run `sleep infinity` and never contain user code.
2. Stream the source into `/work` over exec stdin.
3. For compiled languages, compile under the compile memory and process budget, then empty `/tmp` and lower the limits to the run budget with `ContainerUpdate`.
4. Run with stdin attached, under the timeout. On timeout or an output flood, kill the container rather than waiting for the process.
5. Detect memory-limit kills from the cgroup's `memory.events` (`State.OOMKilled` doesn't fire for exec'd processes).
6. Return the result, and remove the container in the background. A refill goroutine starts its replacement.

```
cmd/cic/            wiring, startup checks, graceful shutdown
internal/api/       HTTP handlers and validation
internal/config/    languages.yaml loading
internal/pool/      warm pool: buffered channel of idle containers + refill goroutine per language
internal/runner/    Docker SDK: create, exec, limits, output capture, OOM detection
internal/dockerproxy/  filtering Docker socket proxy (cmd/cic-dockerproxy), used by compose.yaml
images/             cic-go image (Go with a pre-built, read-only standard library cache)
tests/security/     hostile programs run through the full stack
bench/              latency benchmark and load test
```

The reasoning behind each design choice is in [docs/decisions/](docs/decisions/), the original plan is in [docs/PRD.md](docs/PRD.md), and [docs/writeups/go-cache-time-bomb.md](docs/writeups/go-cache-time-bomb.md) tells the story of the strangest bug: Go builds that broke themselves 24 hours after the image was built.

## Tests

```sh
go test -race ./...
```

Most tests are integration tests against the real Docker daemon. They cover each language's success and error paths, limits, timeouts, memory kills in both phases, the warm pool, and `tests/security/`, where fork bombs, memory bombs, output floods, and escape probes all have to come back with a controlled status. They are skipped if Docker or the images aren't available.

## Benchmarks

`./scripts/bench.sh` writes [bench/results.md](bench/results.md): cold vs warm latency for each language (100 sequential runs each of hello world and a 1-second compute loop), and a 5-minute load test with 50 concurrent clients that fails on any internal error, leaked container, or goroutine growth.

Latest run (16 CPUs, Docker 29.9.2), request latency in ms. Compiled languages include compile time:

| Language | Program | Cold p50 | Cold p95 | Warm p50 | Warm p95 | p50 saved |
|---|---|---:|---:|---:|---:|---:|
| python | hello world | 272 | 304 | 131 | 153 | 141 (52%) |
| python | 1 s compute | 1293 | 1589 | 1131 | 1136 | 162 (13%) |
| node | hello world | 286 | 309 | 146 | 154 | 140 (49%) |
| node | 1 s compute | 1290 | 1307 | 1145 | 1151 | 145 (11%) |
| c | hello world | 426 | 450 | 278 | 288 | 148 (35%) |
| c | 1 s compute | 1430 | 1455 | 1280 | 1292 | 150 (10%) |
| rust | hello world | 540 | 570 | 387 | 396 | 153 (28%) |
| rust | 1 s compute | 1536 | 1555 | 1395 | 1404 | 141 (9%) |
| go | hello world | 877 | 1087 | 598 | 634 | 279 (32%) |
| go | 1 s compute | 1747 | 1784 | 1604 | 1647 | 143 (8%) |

The pool saves a near-constant ~145 ms (container create and start) per request, more for Go. That's half the latency of an interpreted hello world, and it also tightens the tail: Python's 1 s compute p95 is 1136 ms warm against 1589 ms cold.

Load test (50 clients, 5 minutes, a mix of normal programs, timeouts, memory bombs, floods, fork bombs, and compile errors): 3,199 requests, 0 internal errors, 0 wrong statuses, goroutines 11 before and after, 0 containers left. It is not fast under that load, though: about 11 requests/s, p50 4.9 s, and only 20% warm. With 50 concurrent clients and 2 idle containers per language, most requests cold-start, and container creation through the Docker daemon becomes the bottleneck. Higher throughput would need a bigger pool, parallel refills, or a cap on concurrent cold starts.

Docker SDK: `github.com/moby/moby/client` v0.6.2 (API v1.56), pinned in `go.mod`.
