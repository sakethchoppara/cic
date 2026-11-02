# Benchmark results

2026-10-30, 16 CPUs, Docker 29.9.2, go1.27.3.

## Cold vs warm latency

100 sequential runs per cell; total request latency in ms (includes compile for C, Rust, Go).

| Language | Program | Cold p50 | Cold p95 | Warm p50 | Warm p95 | p50 saved |
|---|---|---:|---:|---:|---:|---:|
| python | hello world | 272 | 304 | 131 | 153 | 141 (52%) |
| python | 1 s compute | 1293 | 1589 | 1131 | 1136 | 162 (13%) |
| node | hello world | 286 | 309 | 146 | 154 | 140 (49%) |
| node | 1 s compute | 1290 | 1307 | 1145 | 1151 | 145 (11%) |
| c | hello world | 426 | 450 | 278 | 288 | 148 (35%) |
| c | 1 s compute | 1430 | 1455 | 1280 | 1292 | 150 (10%) |
| rust | hello world | 540 | 570 | 387 | 396 | 153 (28%) |
| rust | 1 s compute | 1536 | 1555 | 1395 | 1404 | 141 (9%) |
| go | hello world | 877 | 1087 | 598 | 634 | 279 (32%) |
| go | 1 s compute | 1747 | 1784 | 1604 | 1647 | 143 (8%) |

## Load test

```
2026/10/31 01:06:21 load: 50 workers for 5m0s; 11 goroutines, 10 containers (idle pools)
requests: 3199 in 5m0s (10.7/s), warm: 652 (20%)
  compile_error    130
  memory_exceeded  125
  ok               2593
  runtime_error    140
  timeout          211
latency ms: p50 4910, p95 6899, p99 8961
internal errors: 0
wrong status: 0
goroutines: 11 before, 11 after
containers: 10 before, 10 after (pools full), 0 after Close
PASS
```
