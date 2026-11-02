# 2. Docker socket access

**Decision:** The runner is a host binary that talks to the local Docker socket directly. It does not run in a container, and no container it starts ever gets the socket.

**Context:** Anything that can talk to the Docker daemon can start a privileged container that mounts `/`, so access to the socket means root on the host. This matters more than any container escape. The options were:
1. Direct socket access from the host.
2. A socket proxy that allows only the API calls CIC needs.
3. Rootless Docker.

**Why direct access for v1:** It's the simplest setup that keeps the socket away from user code, and the project targets a single-user dev machine. Two things limit the risk:
- The runner builds every create request from the trusted config, never from request data. A request can choose only the language (looked up in a map), the code and stdin (sent over exec stdin, never into container options), and a timeout and memory limit, both bounds-checked.
- `tests/security/testdata/host_paths.py` checks that no Docker or containerd socket is visible inside a run.

**Consequences:**
- A bug that let request data reach container options would be a host compromise. Any change to `runner.start` deserves extra scrutiny.
- The runner can't ship in Docker Compose without a proxy. Mounting the socket into the runner's own container would contradict this decision.
- **Next step:** a socket proxy allowing only create, start, exec (create, start, inspect), update, kill, list, and remove, and rejecting `Privileged`, binds, and host namespaces in create bodies. Rootless Docker is the stronger option if the host allows it.

**Update (Week 8): socket proxy built.** `cic-dockerproxy` (`internal/dockerproxy`) now sits between the runner and the daemon in the Compose setup. Decisions made while building it:
- **Allowlist, not blocklist.** A create body may only contain the options the runner sets. Any other key with a non-zero value is refused, so new Docker features are denied by default. "Zero" is defined carefully: the client sends unused structs like `"RestartPolicy": {"Name": ""}`, but in `"Volumes": {"/data": {}}` the empty object is the point, so a nested empty object or array counts as non-zero. A unit test caught the first version letting that volume through.
- **Label check by inspecting the container**, cached per container ID, so start, kill, update, exec, and remove only touch `cic` containers. Measured latency is unchanged within noise.
- **Exec start and inspect aren't label-checked:** exec IDs are 64 hex characters and only come from checked exec creates.
- **Reject `..` and escaped paths outright,** rather than trying to predict how the daemon normalizes them.
- **Written in Go rather than using an off-the-shelf proxy:** HAProxy-based proxies filter by endpoint, but they can't inspect a create body for `Privileged` or binds, which is the attack that matters.
