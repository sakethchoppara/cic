# PRD: Code-In-Container (CIC), Sandboxed Multi-Language Code Runner (Go)

## 1. Overview

**Project name:** `cic` (Code-In-Container)

**One-line summary:** A sandboxed code execution service written in Go that accepts source code in multiple languages and returns output, errors, and timing, backed by a warm pool of isolated Docker containers.

**Owner:** Saketh Choppara

**Status:** Planning, revised (rev 3: exec-model fixes, see section 15)

**Target completion:** 8 weeks at 8 to 10 hours per week, including one buffer week for benchmarks and documentation

## 2. Goals

1. Run untrusted code in isolated, resource-limited containers with no network access by default.
2. Reduce execution latency with a warm container pool, and measure the difference against cold starts.
3. Support at least five languages through a single, uniform runner interface.
4. Use Go's concurrency primitives (goroutines, channels, `context`) as the core of the pool and request handling.
5. Publish a reproducible benchmark of cold vs warm latency per language.
6. Document the threat model honestly, including the parts that are not sandboxed.
7. Produce a public repo and deployment a recruiter can try in under five minutes.

## 3. Non-goals

- Interactive or stateful sessions (notebooks, REPLs that persist variables across runs).
- GPU workloads and ML model execution.
- Multi-node deployment, Kubernetes, or autoscaling.
- User accounts, billing, or a production web IDE front end (a minimal test page is enough).
- Competing with Judge0 or Piston on language count.
- Replacing Docker with a custom container runtime.
- Hardening the Docker daemon itself. Section 8.1 covers how the service accesses it and documents the residual risk.
- Judging submissions against hidden test cases (the LeetCode/HackerRank model). CIC runs code and returns output; a harness layer is future work.

## 4. Users

- **Primary:** Saketh, learning Go concurrency, sandboxing, container lifecycle management, and API design.
- **Secondary:** Engineering hiring managers reviewing the repo, the benchmark, and the security write-up.
- **Tertiary:** Developers who call the API from a script or a small web page.

## 5. Supported languages (v1)

| Language | Base image | User | Compile step | Run command | Compile memory | Run memory |
|----------|-----------|------|--------------|-------------|----------------|------------|
| Python 3.12 | `python:3.12-slim` | `nobody` | none | `python3 main.py` | n/a | 256 MB |
| Node.js 20 | `node:20-slim` | `node` | none | `node main.js` | n/a | 256 MB |
| C (gcc) | `gcc:13` | `nobody` | `gcc -O2 main.c -o main` | `./main` | 512 MB | 256 MB |
| Rust | `rust:1.80-slim` | `nobody` | `rustc -O main.rs -o main` | `./main` | 1024 MB | 256 MB |
| Go | `cic-go:1.22` (from `golang:1.22`) | `nobody` | `go build -o main main.go` | `./main` | 1024 MB | 256 MB |

Every language entry in `config/languages.yaml` sets these fields: `image`, `user`, `source_file`, `compile_cmd` (optional), `run_cmd`, `env` (writable cache paths), `compile_memory_mb`, `run_memory_mb`, `compile_pids` (optional), and `tmpfs_mb`. Adding a language requires one entry and no code changes. The image must have the named user, and the runner rejects any language whose image fails a startup check that the user exists.

Compiled languages get larger `tmpfs_mb` values than the 16 MB originally proposed, since `rustc` and `go build` temp output need more space than that.

Go uses a derived image, `cic-go:1.22` (`images/go.Dockerfile`), with the standard library pre-built into a read-only `GOCACHE=/gocache`. With an empty cache, every run compiles the standard library from scratch: 6 to 10 seconds on 1 vCPU, past the compile timeout. With the pre-built cache, hello world with `fmt` compiles in about 350 ms. `go build` tolerates a cache it can't write to, and user code can't poison the cache. `scripts/images.sh` pulls and builds every image.

Because the root filesystem is read-only and the user is not root, `env` must point every cache and temp path at tmpfs. At minimum: `HOME=/tmp` for all languages, `GOCACHE=/tmp/gocache` and `GOPATH=/tmp/gopath` for Go, and `TMPDIR=/tmp` for Rust and C. Verify in Week 3.

## 6. API

**`POST /v1/execute`**

Request:
```json
{
  "language": "python",
  "code": "print(sum(range(10)))",
  "stdin": "",
  "limits": { "timeout_ms": 5000, "memory_mb": 256 }
}
```

Response:
```json
{
  "status": "ok",
  "stdout": "45\n",
  "stderr": "",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false,
  "timings": { "queue_ms": 2, "compile_ms": 0, "run_ms": 31, "total_ms": 40 },
  "warm": true
}
```

Possible `status` values: `ok`, `compile_error`, `runtime_error`, `timeout`, `memory_exceeded`, `internal_error`.

`memory_exceeded` is set when the container's cgroup records an OOM kill during a phase (see FR7, step 5). The response says which phase hit the limit in a `phase` field (`compile` or `run`).

**`GET /v1/languages`** returns the supported language list.

**`GET /v1/health`** returns pool size, busy count, and queue depth per language.

Limits: code up to 64 KB, stdin up to 64 KB, stdout and stderr each capped at 64 KB with a truncation flag. Requests above the size limits get HTTP 413. Unknown languages get HTTP 400.

## 7. Functional requirements

**FR1: Execution endpoint.** Accept a JSON request, decode it into a typed struct, validate required fields and size limits, route to the correct language config, run it, and return the response. Reject unknown languages with HTTP 400 and oversized payloads with HTTP 413.

**FR2: Isolated execution.** Each run happens in a container that is destroyed after the run. Nothing carries over to the next run, including files, processes, and environment. There is no shared writable state between runs.

**FR3: Network isolation.** Containers start with `NetworkMode: none`. No outbound network access in v1.

**FR4: Resource limits.** Apply to every run via the Docker SDK host config:
- CPU: 1 vCPU (`NanoCPUs`)
- Memory: a cgroup limit applies to the whole container, not to each exec. The container is created with the compile budget (or the run budget for interpreted languages). After a successful compile, the runner calls `ContainerUpdate` to lower `Memory` and `MemorySwap` to the run budget before starting the run exec. This is safe because the compile processes have exited by then, so usage is near zero. `MemorySwap` always equals `Memory` to disable swap. Users can request up to 512 MB for run and cannot exceed the language's compile budget for compile.
- Process count: `PidsLimit` of 64 for run. This limit counts threads, not just processes, and `go build` and `rustc` start many threads. The compile phase uses `compile_pids` (default 256), lowered to 64 by the same `ContainerUpdate` call.
- Wall-clock timeout: default 5 seconds, max 15 seconds. The timeout is a `context.WithTimeout`. On expiry, the runner kills the container. It does not wait for the process to exit.
- Disk: `ReadonlyRootfs: true` with `tmpfs` mounts at `/tmp` and `/work`, sized per language in `tmpfs_mb`. The `/work` mount must use the `exec` option (tmpfs is `noexec` by default, so `./main` would fail) and mode `1777` so the non-root user can write to it.

**FR5: Least privilege.** Run as the non-root user named in the language config. Drop all Linux capabilities (`CapDrop: ["ALL"]`). Set `no-new-privileges` in `SecurityOpt`. Use the default seccomp profile and do not disable it.

**FR6: Warm pool.** Keep N idle containers per language (configurable, default 2), each started with a long-lived idle command and held in a buffered channel. A request takes a container from the channel and runs the exec flow described in FR7. After the run, the container is destroyed and a refill goroutine starts a replacement and pushes it onto the channel. Refill runs in the background and never blocks requests. If the channel is empty, the request waits up to a configurable queue timeout via `select` on the channel and a timer. If the wait times out, the request falls back to a cold start and sets `"warm": false`.

**FR7: Exec flow.** Each run follows these steps in order:
1. Take an idle container from the pool, or cold-start one.
2. Write the source file into `/work` by attaching to an exec of `sh -c 'cat > /work/<source_file>'` and streaming the code over stdin. Do not use bind mounts. Do not use the Docker archive (copy) API: it does not write into tmpfs mounts and fails against a read-only root filesystem. Verify this in Week 2 before relying on either approach.
3. If the language has a compile step, exec the compile command under its own `context` timeout (default 10 seconds) with the compile memory and pids budgets. Report compile failures with stderr and `status: compile_error`. Then lower the limits to the run budgets (FR4).
4. Exec the run command under the run timeout, with stdin attached.
5. After each phase, read `oom_kill` from `/sys/fs/cgroup/memory.events` inside the container (cgroup v2, private cgroup namespace). If the count went up during the phase, map the result to `memory_exceeded` with that `phase`. Do not use `State.OOMKilled`: it only reflects the container's main process, and the idle command keeps running when an exec'd process is killed. Exit code 137 alone is not enough either, because a timeout kill also produces it.
6. Capture output, map the result to a status, and destroy the container.

The idle command is `sleep infinity` or an equivalent that does nothing. The source file is never present in an idle container.

Between steps 3 and 4, the runner empties `/tmp`. tmpfs pages are charged to the container's memory cgroup, so leftover build temp files would otherwise eat into the run budget after the limit is lowered.

**FR8: Output handling.** Capture stdout and stderr separately using Docker's multiplexed stream (`stdcopy.StdCopy`) into writers that stop accepting bytes once they reach 64 KB. When a stream hits the cap, set its truncation flag and keep reading only to drain the stream. If the combined output of both streams passes a second, higher threshold (256 KB), kill the container and return `runtime_error` with `stdout_truncated` set. This keeps the host from buffering a flood, and the flood test exercises the path directly.

**FR9: Cleanup.** On graceful shutdown (SIGINT or SIGTERM), cancel the root context, drain the pools, and remove all pooled containers. On startup, remove any leftover containers carrying the `cic` label from a previous run.

**FR10: Health endpoint.** Report pool size, busy count, and queue depth per language from atomic counters.

## 8. Non-functional requirements

### 8.1 Threat model and Docker socket access

The runner needs access to the Docker daemon. Anything that can talk to the Docker socket can typically start a privileged container that mounts the host filesystem, which means it has root on the host. This is a larger risk than any container escape, and `SECURITY.md` must address it directly.

Requirements for v1:
- Run the Docker socket access from a dedicated user or service, not from an interactive account.
- Do not mount the Docker socket into any container the runner launches. If the runner itself runs in Docker, it reaches the daemon only through the socket proxy (below), never through a raw socket mount.
- Document the chosen approach in `docs/decisions/` and in `SECURITY.md`. The options are: direct socket access on a dev machine, a restricted socket proxy that allows only the create, start, exec, wait, inspect, update, and remove calls the runner needs, or a rootless Docker daemon. v1 targets direct access from a host binary on a dev machine, with a documented note. The socket proxy is a stretch goal and is required before the runner is shipped in Compose.
- State in `SECURITY.md` that anyone who can reach the runner's HTTP port can submit arbitrary code, and that the API has no authentication in v1. Bind to `127.0.0.1` by default.

### 8.2 Other requirements

- **Security:** The runner must survive a standard set of hostile test programs without escaping the container or affecting the host. Include a `tests/security/` folder with at least: infinite loop, fork bomb, memory bomb (allocate until killed), large stdout flood, large stderr flood, attempts to reach host paths (the container's own `/etc/passwd` is readable and that is expected; the test checks that host files and host-only paths are absent), attempt to write outside `/work` and `/tmp`, attempt to open a network socket, and attempt to read the Docker socket if it is mounted by mistake. Each must return a controlled response with the correct status and `phase` where relevant.
- **Correctness:** At least 40 tests: one happy-path and one error-path per language, plus limits, OOM mapping, isolation, and truncation tests. Run with `go test ./...` and `go test -race ./...` in CI. The race detector must be clean.
- **Latency:** Publish cold vs warm p50 and p95 per language over 100 runs of a hello-world and a 1-second compute program, generated by a Go benchmark or script.
- **Reliability:** Under 50 concurrent requests for 5 minutes, no internal errors and no leaked containers (verify with `docker ps` before and after). Also verify that goroutine count returns to baseline after the load.
- **Reproducibility:** On a clean machine with Docker and Go installed, `./scripts/images.sh`, then `go run ./cmd/cic`, followed by `./scripts/bench.sh` works. The service builds with `go build ./cmd/cic` and no external toolchain beyond Go. A `docker compose up` path is added only together with the socket proxy (8.1).
- **Documentation:** README with quick start, API reference, and a `SECURITY.md` describing the threat model, the controls, the Docker socket decision, and known gaps.

## 9. Architecture

```
Client → net/http server (stdlib ServeMux) → Handler → Language Config
                                              ↓
                                        Warm Pool Manager (channels + refill goroutines)
                                              ↓
                                    Runner (Docker SDK, context-based timeouts)
                                              ↓
                  Isolated container (no network, limits, non-root, read-only FS, destroyed after run)
```

The stdlib `ServeMux` supports method and path patterns (`POST /v1/execute`) since Go 1.22, so no router dependency is needed.

Docker SDK: `github.com/moby/moby/client` (the successor to `github.com/docker/docker/client`), pinned in `go.mod`.

Module layout:

```
cmd/cic/               main package: wiring, signal handling, graceful shutdown
internal/api/          HTTP handlers, request decoding, validation, response types
internal/config/       language config loading from languages.yaml, startup image checks
internal/pool/         per-language pool, refill goroutines, queue timeout logic
internal/runner/       container create/start/exec/update/wait/kill/remove, output capture
internal/limits/       resource limit defaults and bounds checks
internal/health/       atomic counters and health handler
bench/                 latency benchmark
tests/security/        hostile program fixtures and test runner
config/languages.yaml  language definitions
docs/decisions/        decision records
```

Packages are created when the milestone that needs them starts, not up front.

Concurrency model, documented in `docs/decisions/`:
- One buffered channel of idle containers per language.
- One refill goroutine per language, fed by a signal channel when a container is consumed.
- Each request runs under a `context` derived from the request context, so a client disconnect cancels the run and frees the container.
- Output capture runs in its own goroutine per stream, and each stream writer has a fixed cap.

## 10. Key design decisions to document

Write a short decision record in `docs/decisions/` for each:

1. **Container vs microVM:** Docker containers for v1 with a note on the kernel-sharing risk. Record what changes if you move to gVisor or Firecracker.
2. **Docker socket access:** Direct access, socket proxy, or rootless daemon, and why. This is the most important record in the set.
3. **Pool sizing:** How you chose N, what happens under burst load, and how queue timeout interacts with it.
4. **Stateless runs:** Why each run is isolated, and what you gave up by not supporting sessions.
5. **Pool refill strategy:** Replace-after-use vs periodic refresh. Record the tradeoff.
6. **Source delivery via exec stdin:** Why source is streamed into an idle container through an exec rather than bind mounts or the archive API, and what that costs in latency.
7. **Timeout and cancellation:** Why timeouts use `context` and why the runner kills the container rather than waiting for the process to exit.
8. **Channels vs mutex-guarded slice for the pool:** Record why you chose channels and where the design could deadlock.
9. **Per-phase limits and OOM detection:** Why limits are lowered with `ContainerUpdate` between compile and run, and why OOM is read from cgroup `memory.events` instead of `State.OOMKilled`.

## 11. Milestones

| Week | Milestone | Done when |
|------|-----------|-----------|
| 1 | Go fundamentals for this project, HTTP server with stub endpoint, Docker SDK container listing | `POST /v1/execute` returns a stub response; Go program lists Docker containers and starts one idle container |
| 2 | Single-container Python runner with the full exec flow | Python code runs via stdin source delivery and exec with timeout, memory, pids, network, read-only FS, and non-root enforced; OOM maps to `memory_exceeded` via `memory.events`; tests pass |
| 3 | Add Node, C, Rust, Go via language config | All five languages return correct output for hello-world; compile errors report correctly; compile-to-run limit lowering works; `env` cache paths verified |
| 4 | Warm pool with channels and background refill | Pool maintains N idle containers; `warm: true` on pooled runs; queue timeout falls back cold; `-race` clean |
| 5 | Security test suite and fixes | All hostile programs return controlled responses with correct phase; flood truncation path exercised; `SECURITY.md` written |
| 6 | Benchmarks, load test, graceful shutdown and cleanup | Cold vs warm table published; 5-minute load test with no leaked containers and no goroutine growth |
| 7 | Buffer week: fix what broke, write the bug write-up, finish decision records | All decision records written; 500-word write-up drafted |
| 8 (stretch) | Minimal web test page, or socket proxy for Docker access (then add `docker compose up`) | Browser page submits code and shows output, or proxy restricts the Docker API to the needed calls |

Week 1 is intentionally small. Start with goroutines, channels, and `context` before touching the pool. The Python runner in Week 2 is the first real test of the design, so protect that week.

## 12. Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| Go learning curve slows the first two weeks | Week 1 scope is limited to a stub endpoint and container listing; spend early days on goroutines, channels, and `context` |
| Docker startup is too slow to be useful | Warm pool; measure early in Week 2 and adjust pool size |
| Compiled languages OOM during compile | Separate compile memory budget per language, lowered for run with `ContainerUpdate`; OOM mapped to `memory_exceeded` with `phase: compile` |
| Read-only root FS breaks build tools | Writable cache paths in `env` for each language; tmpfs sized per language and mounted `exec`; tested in Week 3 |
| Go compiles the standard library on every run | Derived image with a pre-built, read-only `GOCACHE` (section 5) |
| Compilers hit the pids limit | Pids limit counts threads; separate `compile_pids` budget |
| Docker socket gives host-level access | Section 8.1; dedicated user, documented decision, optional socket proxy stretch goal |
| Escape or host impact from a hostile program | Layered controls in FR3 to FR5; document residual risk in `SECURITY.md` |
| Output flood exhausts host memory | Capped stream writers and a hard kill threshold (FR8); flood tests in Week 5 |
| Leaked containers after crashes | Labels plus startup cleanup (FR9); check in load test |
| Goroutine leaks or deadlocks in the pool | Run `-race` in CI; test goroutine count returns to baseline after load |
| Docker SDK API churn | Pin the SDK version in `go.mod`; note the version in the README |
| Scope creep into sessions or ML | Non-goals in section 3; log ideas in `FUTURE.md` |

## 13. Success criteria

- `go test ./...` and `go test -race ./...` pass in CI with 40+ tests.
- Published cold vs warm latency table in the README, generated by a script.
- All security test programs return controlled responses with the correct status and phase.
- `SECURITY.md` names the threat model, the Docker socket decision, and at least three known gaps.
- Zero leaked containers and no goroutine growth after the 5-minute load test.
- One 500-word write-up on a bug you hit, most likely in the pool, timeout, or cancellation logic. This is your strongest interview material.

## 14. Resume entry (draft, fill in after completion)

**CIC (Code-In-Container): Sandboxed multi-language code execution service in Go** (github link)
Built a Go service that runs untrusted code in isolated, destroy-after-use Docker containers across five languages. Enforced CPU, memory, process, network, and filesystem limits with non-root execution and capability drops, and added a channel-based warm container pool that cut p50 latency from [X] ms to [Y] ms. Verified isolation with a hostile-program test suite and a [N]-minute concurrency load test with a clean race detector run.

## 15. Revision 3 changes

1. Compile and run memory budgets: one cgroup per container, so limits are lowered with `ContainerUpdate` between phases (FR4).
2. OOM detection: cgroup `memory.events` per phase instead of `State.OOMKilled` (FR7.5).
3. Source delivery: exec stdin instead of the archive API, which doesn't write into tmpfs (FR7.2).
4. `/work` tmpfs mounted with `exec` (FR4).
5. Compose vs socket rule: Compose only ships with the socket proxy (8.1, 8.2).
6. Pids limit counts threads; separate compile budget (FR4).
7. Host-path security test reworded; container `/etc/passwd` is expected to be readable (8.2).
8. Router: stdlib `ServeMux` instead of chi; Docker SDK is `github.com/moby/moby/client`.
