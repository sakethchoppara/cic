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

**v1 decision:** the runner is a host binary that uses the local Docker socket directly, on a single-user development machine. It is not run in a container, and the socket is never mounted into any container it starts (`host_paths.py` checks this).

Why: it's the simplest setup that keeps the socket out of reach of user code. The runner only makes create, start, exec, update, kill, inspect, list, and remove calls, with arguments built from the trusted config, never from the request.

Operating it more safely, in order of effort:
1. Run it as a dedicated user in the `docker` group, not your own account.
2. Put a socket proxy in front of the daemon that allows only the calls above, and denies `Privileged`, bind mounts, and host namespaces in create requests. This is required before the runner ships in Docker Compose.
3. Use rootless Docker, so "root on the daemon" is an unprivileged user on the host.

## Known gaps

These are real, unmitigated in v1, and roughly in order of severity.

1. **Shared kernel.** Containers share the host kernel. A kernel exploit reachable through the syscalls the default seccomp profile allows escapes everything above. gVisor (`runsc`) or Firecracker microVMs would close this; gVisor is a drop-in Docker runtime.
2. **No authentication, rate limiting, or concurrency cap.** Anyone who can reach the port can run code. When the warm pool is empty, every request cold-starts its own container, so N concurrent requests means N containers, each allowed up to 512 MB (1 GB while compiling) and 1 vCPU for up to 15 s. A modest flood exhausts host memory or CPU. Keep the default `127.0.0.1` bind until a global semaphore and authentication exist.
3. **The Docker socket is root.** See above. A bug in the runner that lets request data reach container create options would be a host compromise.
4. **No user namespace remapping.** UID 65534 in the container is UID 65534 on the host. Combined with gap 1, an escape lands as host `nobody` rather than an unmapped UID. Docker's `userns-remap` would add that layer.
5. **Information disclosure.** `/proc` and `/sys` reveal the host kernel version, CPU model, memory size, and uptime. Timing and cache side channels between concurrent runs are not addressed.
6. **No disk I/O limits.** There are no blkio limits, but no persistent writable disk is reachable either; tmpfs is in memory and counted against the memory limit.
7. **Image supply chain.** Images are pinned by tag, not digest, so a re-pulled tag can change. Pin digests for production.

## Reporting

This is a portfolio project, not a production service. Report issues through GitHub issues on this repository.
