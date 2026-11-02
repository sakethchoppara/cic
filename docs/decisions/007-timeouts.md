# 7. Timeouts and cancellation

**Decision:** Every phase runs under a `context.WithTimeout` derived from the request's context. When the context ends, the runner closes the exec's attach connection and then kills the whole container. It never waits for the process to exit.

**Why `context`:** One mechanism covers three cases: the phase timeout, the client disconnecting (the HTTP server cancels `r.Context()`), and server shutdown. The runner checks which one fired: a phase deadline becomes `timeout`, while a parent cancellation is reported as an error and not answered.

**Why kill the container rather than the process:**
- The program may have forked (the fork bomb starts up to 64 processes), ignored signals, or be blocked in the kernel. Killing the container's PID 1 tears down the whole PID namespace in one call.
- The container is about to be destroyed anyway (record 4), so there's no state to preserve.

**The bug that shaped the order:** The first version killed the container and *then* closed the attach connection. An output flood took 10 s to stop: the daemon was blocked pushing output into a connection nobody was reading, and the kill waited for it (10 s happens to be Docker's stop timeout). Closing the connection first brought that down to 0.75 s. A "noisy timeout" test (a program that prints while it loops) now guards the ordering, because a silent infinite loop never triggered it.

**Consequences:** Output printed before a timeout is still returned (`timeout_keeps_output.py`). Removal of the container always uses a fresh context, so cleanup still happens when the request context is already done.
