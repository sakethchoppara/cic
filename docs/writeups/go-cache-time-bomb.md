# The Go build that broke itself after 24 hours

Every Go request to CIC compiles with `go build` inside a locked-down container: 1 vCPU, no network, and a read-only root filesystem. Since Go 1.20 the standard library isn't shipped pre-compiled, so with an empty build cache every request compiled `fmt` and its dependencies from scratch. That took over 10 seconds, past the compile timeout.

The fix looked clean. I built a derived image that runs `go build std` into `GOCACHE=/gocache` and leaves the cache read-only at run time. `go build` treats cache writes as best-effort, so it read the cache, silently dropped its own writes, and hello world compiled in 350 ms. User code also couldn't poison the cache for the next run. Every test passed, and I committed it.

The next day, every Go test failed:

```
go: failed to trim cache: open /gocache/trim.txt: permission denied
```

Nothing in CIC had changed. The image hadn't changed either. Only the calendar had.

I read `cmd/go/internal/cache/cache.go` inside the image instead of guessing. `DiskCache.Trim` runs whenever a build finishes. It reads `trim.txt`, and if the last trim was less than 24 hours ago it returns immediately. Otherwise it walks the cache's 256 subdirectories, deletes entries unused for 5 days, and writes the current time back to `trim.txt`. The deletions ignore errors, but the `trim.txt` write doesn't, and its error ends the build. My image's `trim.txt` was written at build time, so for exactly 24 hours every build took the early return. After that, every build tried to trim and failed on the write.

The tests didn't miss this because they were weak. They missed it because the failure depended on how old an artifact was, and the tests ran right after the artifact was built. Any time-based behaviour in a dependency creates a test gap like that.

I considered three fixes:
- **A far-future timestamp in `trim.txt`.** It looked like the cheapest fix, but the same function rejects timestamps more than an hour in the future, a guard against corrupt files. Reading the source first saved me from shipping a second time bomb.
- **A writable cache.** Each container would need its own copy (slower), or user code could modify a cache that later runs trust.
- **A symlink from `/gocache/trim.txt` to `/tmp/go-trim.txt`,** which is what I shipped. `/tmp` is a fresh, writable tmpfs in every container. A new container has no `/tmp/go-trim.txt`, so Go always takes the trim path: the walk's deletions fail silently against the read-only cache, and the final write lands in `/tmp`. The cache stays read-only, and builds still take about 350 ms.

The symlink also turned a test I couldn't run into one that runs on every build. The code path that only ran after 24 hours now runs on every compile, so the ordinary Go hello-world tests cover it. Three days after the image was built, the full suite still passes.

What I took from it:
- When a tool's cache is read-only, check which of its writes are best-effort and which are fatal. The difference is often a single unchecked error.
- If behaviour depends on time, don't wait for the time to pass. Make the time-dependent path run every time, so ordinary tests exercise it.
- A failure that appears with no code change usually means something external moved: a clock, a cache, a certificate, a tag.
