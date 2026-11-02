# Security

CIC runs untrusted code. This document says what it defends against, how, how that is tested, and where it is weak. Read the [known gaps](#known-gaps) before exposing it to anyone you don't trust.

## Threat model

**Attacker:** anyone who can reach the HTTP port. They control the source code, stdin, and the requested limits (within bounds), in any supported language, and can send as many requests as they like.

**What they want:**
1. Escape the container: read or write host files, run processes on the host, reach the Docker daemon.
2. Reach the network: the internet, the host, or other services on the host's networks.
3. Take down the service or the host: exhaust memory, CPU, PIDs, disk, or the runner's own memory.
4. Affect other users' runs: read their code or output, or leave something behind for the next run.

**Trusted:** the host, the Docker daemon, the images listed in `config/languages.yaml`, the config file, and the operator.

**The most important fact in this document:** the runner talks to the Docker daemon, and anything that can talk to the Docker daemon is effectively root on the host. It can start a privileged container that mounts `/`. The runner process is therefore as sensitive as root, and any bug that lets a request influence the Docker calls it makes (image, mounts, privileges) is a host compromise. See [Docker socket access](#docker-socket-access).

## Controls

Every run gets a fresh container that is destroyed afterwards, warm or cold. The settings are in `internal/runner/runner.go` (`start`).

| Threat | Control |
|---|---|
| Container escape via privileges | Non-root user (`nobody`, or `node` for Node), `CapDrop: ALL`, `no-new-privileges`, Docker's default seccomp profile |
| Host filesystem access | No bind mounts or volumes. Source is streamed in over exec stdin. Read-only root filesystem; only `/work` and `/tmp` (tmpfs) are writable |
| Network access | `NetworkMode: none`: only a loopback interface exists |
| Memory exhaustion | cgroup `Memory` with `MemorySwap` equal to it (no swap): 256 MB run by default, at most 512 MB; compile budgets per language. tmpfs pages count against the same limit |
| Fork bombs | `PidsLimit` 64 during run (counts threads), 256 during compile |
| CPU exhaustion | 1 vCPU per container; wall-clock timeout (default 5 s, max 15 s run; 10 s compile) that kills the container instead of waiting for the process |
| Disk exhaustion | tmpfs `size=` per language (16 to 128 MB), counted against memory |
| Output floods | Each stream is capped at 64 KB in memory; past 256 KB combined, the container is killed |
| Request size | Code and stdin at most 64 KB each; JSON body capped; unknown fields rejected |
| Request floods | At most `-max-runs` executions at once across all languages (default: CPU count); others wait up to 10 s, then get HTTP 503 with `Retry-After` |
| Cross-run leakage | Containers are never reused; idle pooled containers never contain user code; the Go build cache is read-only, so code can't poison it for later runs |
| Leaked containers | Removed after every run with a fresh context (even on timeout or client disconnect); leftover `cic`-labelled containers removed at startup; pools drained on shutdown |
| Exposure | Binds to `127.0.0.1:8088` by default |

Memory-limit kills are detected from the container cgroup's `memory.events` (`oom_kill`) per phase, not from `State.OOMKilled`, which never fires for exec'd processes. Limits are lowered from the compile to the run budget with `ContainerUpdate` after compiling, so a program can't use the compiler's larger budget.

## How it's tested

`tests/security/` runs hostile programs through the full HTTP stack with warm pools, then checks that every language still works and that no containers were left behind:

| Program | Expected |
|---|---|
| `infinite_loop.rs` | `timeout`, run |
| `fork_bomb.c` | `timeout`, run (pids limit holds; the host is unaffected) |
| `memory_bomb.{py,js,go}` | `memory_exceeded`, run |
| `compile_bomb.c` (`#include "/dev/zero"`) | `memory_exceeded`, compile |
| `stdout_flood.c`, `stderr_flood.py` | `runtime_error`, truncated, container killed |
| `timeout_keeps_output.py` | `timeout`, output printed before the kill is returned |
| `host_paths.py` | own PID and UTS namespaces; no Docker or containerd socket; `/etc/shadow` unreadable; non-root; no effective or bounding capabilities; `no_new_privs`; seccomp filtering; read-only root and cgroup filesystems; no unexpected mounts |
| `write_outside.py` | writes fail everywhere except `/work` and `/tmp` |
| `tmpfs_fill.py` | `/work` fills up at its size limit |
| `network.js` | only `lo`; TCP to the internet and to the Docker bridge gateway fail; DNS fails; UDP fails |
| `privileges.c` | `setuid(0)`, `setgid(0)`, `mount`, `chroot`, `unshare`, raw sockets, `mknod`, and `sethostname` are all refused |

The checks that print `SAFE` were confirmed to print `UNSAFE` in a deliberately insecure container (root, Docker socket mounted, default network, `--privileged`), so they can fail.

`internal/runner` and `internal/pool` have further tests for the same controls in isolation, including limit lowering between phases and per-request memory limits on warm containers.

## Docker socket access

Anything that can talk to the Docker daemon can start a privileged container that mounts `/`, which is root on the host. CIC supports two setups:

**Development (`go run ./cmd/cic`):** the runner uses the local Docker socket directly. The socket is never mounted into any container it starts (`host_paths.py` checks this). The runner builds every create request from the trusted config, never from request data. Even so, a bug in that code would be a host compromise. Run it as a dedicated user in the `docker` group, not your own account.

**Compose (`docker compose up`), and anywhere else the runner shouldn't be trusted with root:** the runner reaches the daemon only through `cic-dockerproxy` (`internal/dockerproxy`). The proxy runs in its own container with no network, a read-only filesystem, and no capabilities, and it is the only thing that gets the host socket. It forwards only what the runner needs:

| Call | Allowed when |
|---|---|
| ping, version, image inspect | always (read-only) |
| container create | every sandbox setting is present: an image from `languages.yaml`, a non-root user, the `cic` label, network `none`, read-only root, `CapDrop: ALL`, exactly `no-new-privileges`, at most 1 CPU, memory and pids limits within the config's largest budgets, no swap, tmpfs only at `/tmp` and `/work` with a size. **Any other option with a non-zero value is refused**, so privileged mode, bind mounts, volumes, devices, host namespaces, sysctls, runtimes, port bindings, and options added to Docker in the future are all denied. |
| start, kill, update, exec, remove | the container carries the `cic` label. Update may only change memory, swap (equal to memory), and pids, within the same bounds. Exec must name a non-root user and may not be privileged. |
| exec start, exec inspect | always: exec IDs are unguessable and only come from checked exec creates |
| container list | filtered to `cic` labels |
| anything else (pull, build, archive copy, attach, commit, volumes, networks, swarm, plugins) | never |

Paths containing `..` or escaped characters are refused, so a path that passes the checks can't mean something else to the daemon. Every refusal is logged. Unit tests cover 39 create-time attacks and 23 other calls, and an integration test runs the real runner through the proxy against the real daemon, then checks that a privileged create with `/` mounted, an unfiltered list, an image pull, and removing or exec'ing into a non-`cic` container are all refused. The full test suite and a load test pass through the proxy binary with no denials, and latency is unchanged within noise.

With the proxy, a compromised runner can still start sandboxed containers (up to the `-max-runs` budget it is honest about, or more if it isn't), stop or remove CIC's own containers, and run code in them as a non-root user. It can't escape to the host through Docker.

Rootless Docker would add another layer: even the proxy's "root on the daemon" would be an unprivileged host user.

## Known gaps

These are real, unmitigated in v1, and roughly in order of severity.

1. **Shared kernel.** Containers share the host kernel. A kernel exploit reachable through the syscalls the default seccomp profile allows escapes everything above. gVisor (`runsc`) or Firecracker microVMs would close this; gVisor is a drop-in Docker runtime.
2. **No authentication or per-client rate limiting.** Anyone who can reach the port can run code. The `-max-runs` cap bounds how many containers run at once, but not who uses them: one client can hold every slot and starve everyone else (they get 503). Size the cap to memory too: each run may use up to 512 MB, or 1 GB while compiling, so 16 slots can still reach 16 GB in the worst case. Keep the default `127.0.0.1` bind.
3. **The Docker socket is still root, one hop away.** With the proxy, it's held by a small, networkless process instead of the runner. A bug in the proxy's checks, or a Docker API change that adds a dangerous effect to a call the proxy allows, would undo that. Without the proxy (the development setup), a bug in the runner that lets request data reach container options is a host compromise. Nothing limits how many containers a compromised runner starts through the proxy.
4. **No user namespace remapping.** UID 65534 in the container is UID 65534 on the host. Combined with gap 1, an escape lands as host `nobody` rather than an unmapped UID. Docker's `userns-remap` would add that layer.
5. **Information disclosure.** `/proc` and `/sys` reveal the host kernel version, CPU model, memory size, and uptime. Timing and cache side channels between concurrent runs are not addressed.
6. **No disk I/O limits.** There are no blkio limits, but no persistent writable disk is reachable either; tmpfs is in memory and counted against the memory limit.
7. **Image supply chain.** Images are pinned by tag, not digest, so a re-pulled tag can change. Pin digests for production.

## Reporting

This is a portfolio project, not a production service. Report issues through GitHub issues on this repository.
