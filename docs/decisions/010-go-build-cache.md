# 10. A pre-built, read-only Go build cache

**Decision:** Go runs in `cic-go:1.22`, which is `golang:1.22` with the standard library pre-built into `GOCACHE=/gocache` (`images/go.Dockerfile`). The cache is read-only at run time, and `trim.txt` is a symlink into `/tmp`.

**Context:** Since Go 1.20, the standard library isn't shipped pre-compiled. With an empty cache, `go build` compiles it from scratch on every run: 6.6 s for a program with no imports and more than 10 s with `fmt`, past the compile timeout, on 1 vCPU.

**Why read-only:** `go build` tolerates a cache it can't write to (cache writes fail silently), and hello world with `fmt` then compiles in about 350 ms. Read-only also means user code can't poison the cache for later runs. A writable cache would have to be a per-container copy, adding latency and disk use.

**The trim.txt problem:** Once the cache is more than 24 hours old, `go build` trims it and must then write `trim.txt`, and that write is fatal on a read-only cache. Every Go compile started failing one day after the image was built. The symlink sends that write to the container's `/tmp`. See `docs/writeups/go-cache-time-bomb.md`.

**Consequences:**
- `scripts/images.sh` must be run once to build the image, and the `ENV` in the Dockerfile must match the Go entry in `languages.yaml` (`CGO_ENABLED`, `GOTOOLCHAIN`), or cache lookups miss and builds are slow again.
- `go build std` pre-builds every standard library package, and with no network there are no third-party imports, so every compile hits the cache. Only the user's own package is compiled per run.
