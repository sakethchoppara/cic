# 1. Containers, not microVMs

**Decision:** v1 isolates runs with Docker containers using the default runtime (runc).

**Context:** The options were plain containers, gVisor (a user-space kernel that runs as a Docker runtime), and Firecracker microVMs. The goal was a project one person can build in 8 weeks on a laptop, with isolation that is honest about its limits.

**Why:** Containers are the only option that needs nothing beyond Docker. Every control CIC relies on (cgroups, seccomp, capabilities, namespaces, read-only root, tmpfs) is a Docker API field, and the hostile-program suite shows those controls hold against everything short of a kernel exploit.

**Consequences:**
- All runs share the host kernel. A kernel bug reachable through the syscalls Docker's default seccomp profile allows is a full escape. This is known gap 1 in `SECURITY.md`.
- **Moving to gVisor** is mostly configuration: install `runsc`, set `HostConfig.Runtime = "runsc"`. Expect slower syscalls and container starts, which would show up in the benchmark. Reading OOM kills from `memory.events` would need re-checking, since gVisor implements its own cgroupfs.
- **Moving to Firecracker** is a rewrite of `internal/runner`: no Docker exec, no `ContainerUpdate`, and a different way to get code in and output out. The pool design (a channel of ready sandboxes plus a refill goroutine) would carry over unchanged.
