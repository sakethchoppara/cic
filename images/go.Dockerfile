# golang:1.22 with the standard library pre-built into a read-only build cache.
# Without it, every run compiles std from scratch: 6-10s on 1 vCPU, past the compile timeout.
# go build only reads this cache at run time (writes fail silently on the read-only rootfs).
# The ENV here must match the go entry in config/languages.yaml, or cache keys miss.
FROM golang:1.22
ENV GOCACHE=/gocache CGO_ENABLED=0 GOTOOLCHAIN=local
RUN go build std && chmod -R a+rX /gocache
