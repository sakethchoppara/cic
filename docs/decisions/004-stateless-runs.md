# 4. Stateless runs

**Decision:** Every run gets a fresh container that is destroyed afterwards. Nothing persists between runs: no files, processes, or environment.

**Why:**
- **Isolation between users** is structural. There is nothing to clean up incorrectly, because nothing is reused. Idle pooled containers have never seen user code.
- **Hostile programs can't leave anything behind:** no background daemon, no modified file, no poisoned cache. The Go build cache is read-only for the same reason (record 10).
- **Failure handling is simple.** Any error, timeout, or kill ends the same way: remove the container.

**What we gave up:**
- **Sessions:** REPLs and notebooks where variables survive between runs. That would need a container bound to a client, with an idle timeout, a cap on how many exist, and a cleanup policy. It's a different product.
- **Package installs and caches between runs.** No network makes this moot in v1 anyway.
- **Some latency.** Each run pays for one container create (hidden by the pool) and one removal (now done in the background). Reusing containers after a reset would be faster and much harder to make safe.
