# 8. Channels for the pool, not a mutex-guarded slice

**Decision:** Each pool is a buffered `chan string` of idle container IDs plus a buffered `chan struct{}` of owed refills, with one refill goroutine.

**Why:** The operation a request needs is "take an idle container, or wait for one up to a timeout, or give up when the client leaves". That is one `select` over the idle channel, a timer, and `ctx.Done()`. With a mutex and slice it needs a condition variable, and `sync.Cond` can't be combined with a timeout or a context, so it would end up as a polling loop or a hand-built notification channel anyway.

**Where it could deadlock, and why it doesn't:**
- **A send onto `idle` or `refill` blocking forever.** Both buffers are `size`, and idle containers + owed refills + starts in flight never exceed `size`: a container taken from `idle` sends exactly one token, and the refill goroutine consumes a token before it pushes a container. A failed start puts its token back into the slot it just freed.
- **`Close` racing a refill.** `Close` cancels the context, waits for the refill goroutine to exit, and only then drains `idle`, so nothing is pushed after the drain. A start that finishes after cancellation removes its own container instead of pushing it.
- **`Close` racing `Execute`.** If a request took a container after `Close` drained the pool, it would send a refill token nobody reads. That's still safe (the buffer has room) but the container would never be cleaned up by `Close`. The contract is that `Close` runs after in-flight requests finish, which `http.Server.Shutdown` guarantees in `main`.
- **Pool size 0.** Both channels are unbuffered. `idle` never yields a container, so `refill` is never sent to, and every request waits out the queue timeout and goes cold. The benchmark uses this as its cold path.

**Consequences:** Counters for `/v1/health` are atomics alongside the channels. `len(idle)` gives the idle count without locking.
