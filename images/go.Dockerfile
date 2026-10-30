# golang:1.22 with the standard library pre-built into a read-only build cache.
# Without it, every run compiles std from scratch: 6-10s on 1 vCPU, past the compile timeout.
# go build only reads this cache at run time (cache writes fail silently on the read-only rootfs).
# The ENV here must match the go entry in config/languages.yaml, or cache keys miss.
#
# trim.txt: once the cache is over 24h old, go build "trims" it and then must write
# trim.txt, which is fatal on a read-only cache ("failed to trim cache"). The symlink
# sends that one write to /tmp; the trim's own deletions fail silently.
FROM golang:1.22
ENV GOCACHE=/gocache CGO_ENABLED=0 GOTOOLCHAIN=local
RUN go build std && ln -sf /tmp/go-trim.txt /gocache/trim.txt && chmod -R a+rX /gocache
