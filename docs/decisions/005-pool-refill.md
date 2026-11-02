# 5. Refill strategy: replace after use

**Decision:** Each container taken from the pool owes exactly one refill. A per-language goroutine starts the replacement in the background, and used containers are always destroyed.

**Alternatives:**
- **Periodic refresh** (top up the pool on a timer). Simpler to reason about under bursts, but it either polls when idle or lags behind demand, and it decouples the pool's size from what was actually consumed.
- **Reset and reuse** (wipe `/work` and `/tmp`, kill stray processes, return the container to the pool). Faster, but correctness depends on the reset being complete, which is exactly the kind of guarantee a sandbox shouldn't depend on (record 4).

**Why replace after use:** It keeps the invariant simple. Idle containers plus owed refills never exceed the pool size, so neither channel send can block. Refills only happen when something was used, so an idle service does no work.

**Consequences:**
- One goroutine per language starts containers serially, at about 6/s. Under sustained load this is the ceiling (record 3). Several refill workers reading the same token channel would raise it without changing the invariant. This is marked with a `ponytail:` comment in `pool.go`.
- If a refill fails (missing image, daemon hiccup), the token is put back and retried after 1 s, so the pool heals once Docker does.
- Idle containers can age indefinitely. If Docker kills one externally, the next request using it gets `internal_error`. A periodic health check of idle containers would fix that. It hasn't been needed so far.
