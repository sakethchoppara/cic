# 9. Per-phase limits and OOM detection

**Decision:** Each container starts at its language's compile budget for memory and processes. After a successful compile, the runner empties `/tmp` and lowers the limits to the run budget with `ContainerUpdate`. Memory-limit kills are detected per phase from the cgroup's `memory.events` (`oom_kill`), not from `State.OOMKilled`.

**Context:** A cgroup limit applies to the whole container, not to each exec. Compilers need far more than programs should get: rustc and `go build` need up to 1 GB, and they spawn threads, which count against the process limit. A container has one cgroup, and compile and run share it.

**Why lower with `ContainerUpdate`:** By the time compile finishes, its processes have exited, so lowering the limit is safe. A test proves it takes effect: a C program allocating 400 MB fits the 512 MB compile budget, but gets `memory_exceeded` during the run under 256 MB. It needed a `volatile` pointer, because `gcc -O2` optimized away the first version's unused allocation and the test passed for the wrong reason.

**Why empty `/tmp` first:** tmpfs pages are charged to the container's memory cgroup. Compiler temp files left in `/tmp` would silently eat into the program's 256 MB.

**Why `memory.events`:** When an exec'd process is OOM-killed, `sleep infinity` (PID 1) keeps running, so the container never exits and `State.OOMKilled` is never set. Exit code 137 alone can't distinguish an OOM kill from a timeout kill. `oom_kill` in `/sys/fs/cgroup/memory.events` counts kills inside the container (cgroup v2, private cgroup namespace). The runner reads it after each phase and compares it with the previous reading, which also tells it which phase hit the limit.

**Consequences:** One extra exec per phase to read the counter, plus one to clear `/tmp` for compiled languages. Requires cgroup v2.
