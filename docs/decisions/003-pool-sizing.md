# 3. Pool sizing

**Decision:** 2 idle containers per language by default (`-pool-size`), with a 1 s queue timeout (`-queue-timeout`) before falling back to a cold start.

**Context:** An idle container costs memory only for `sleep infinity` (its limits are caps, not reservations), but 5 languages × N containers sit on the host permanently. Starting a container takes about 150 ms (the measured cold vs warm gap), and one refill goroutine per language starts them one at a time.

**How the numbers interact:**
- **Small bursts are absorbed.** In the Week 4 test, 6 simultaneous Node requests against a pool of 2 were all warm. Requests beyond the second waited 150 to 600 ms for refills, which is less than a cold start's total cost plus the queue wait.
- **Sustained load is not.** In the load test, 50 clients produced about 11 requests/s. One refill goroutine supplies about 6 containers/s per language, so the pools drained and 80% of requests went cold, with a p50 of 4.9 s. Container creation through the daemon became the bottleneck.
- **The queue timeout limits how long a waiting request can be held up by a slow refill.** 1 s is long enough to catch a refill (about 150 ms) under light load, and short enough that under heavy load a request isn't stuck for long before going cold.

**Consequences:** Pool size is a capacity knob only up to the refill rate. Past that, adding idle containers just delays when the pool drains. Real capacity under load needs parallel refills, plus a cap on concurrent cold starts. The cap also closes the main denial-of-service gap in `SECURITY.md`.

**Update (Week 8):** `-max-runs` (default: CPU count) now caps executions across all languages. In a 60 s rerun with 50 clients and 16 slots, warm runs rose from 20% to 86%, throughput from 10.7 to 12.2 requests/s, and p99 latency fell from 9.0 s to 6.0 s. Fewer simultaneous creates keep the daemon from thrashing, so refills keep up. The p50 of 4.3 s is now mostly queueing for a slot, which is the intended behaviour under overload.
