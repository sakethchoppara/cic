// bench measures cold vs warm latency per language (-mode latency) or runs a
// sustained concurrent load and checks for errors and leaks (-mode load).
// It runs the service in-process, so it can count goroutines. Needs the images
// from scripts/images.sh.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sakethchoppara/cic/internal/api"
	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/pool"
	"github.com/sakethchoppara/cic/internal/runner"
)

var langOrder = []string{"python", "node", "c", "rust", "go"}

var hello = map[string]string{
	"python": `print("hello")`,
	"node":   `console.log("hello")`,
	"c":      "#include <stdio.h>\nint main(){puts(\"hello\");}",
	"rust":   `fn main(){println!("hello");}`,
	"go":     "package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"hello\")}",
}

// compute spins for one second of wall-clock time.
var compute = map[string]string{
	"python": "import time\ns=time.time()\nn=0\nwhile time.time()-s<1: n+=1\nprint(n>0)",
	"node":   "const s=Date.now();let n=0;while(Date.now()-s<1000)n++;console.log(n>0)",
	"c":      "#include <stdio.h>\n#include <time.h>\nint main(){struct timespec a,b;long n=0;clock_gettime(CLOCK_MONOTONIC,&a);do{n++;clock_gettime(CLOCK_MONOTONIC,&b);}while((b.tv_sec-a.tv_sec)*1000000000L+(b.tv_nsec-a.tv_nsec)<1000000000L);printf(\"%d\\n\",n>0);}",
	"rust":   "use std::time::{Duration,Instant};fn main(){let s=Instant::now();let mut n=0u64;while s.elapsed()<Duration::from_secs(1){n+=1;}println!(\"{}\",n>0);}",
	"go":     "package main\nimport (\"fmt\";\"time\")\nfunc main(){s:=time.Now();n:=0;for time.Since(s)<time.Second{n++};fmt.Println(n>0)}",
}

func main() {
	mode := flag.String("mode", "latency", "latency or load")
	runs := flag.Int("n", 100, "latency: runs per language, program and mode")
	workers := flag.Int("workers", 50, "load: concurrent clients")
	maxRuns := flag.Int("max-runs", runtime.NumCPU(), "load: executions allowed at once (the server's -max-runs)")
	duration := flag.Duration("duration", 5*time.Minute, "load: how long to run")
	cfgPath := flag.String("config", "config/languages.yaml", "language definitions")
	flag.Parse()

	langs, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	r, err := runner.New()
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for _, name := range langOrder {
		if err := r.Check(context.Background(), langs[name]); err != nil {
			log.Fatalf("%s unusable (run scripts/images.sh): %v", name, err)
		}
	}

	switch *mode {
	case "latency":
		latency(r, langs, *runs)
	case "load":
		if !load(r, langs, *workers, *maxRuns, *duration) {
			os.Exit(1)
		}
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

// latency prints a markdown table of end-to-end Execute latency, cold vs warm.
// Runs are sequential so they don't compete for CPU. Warm runs wait for the pool
// to refill first, so every one is genuinely warm.
func latency(r *runner.Runner, langs map[string]runner.Lang, n int) {
	fmt.Printf("%d sequential runs per cell; total request latency in ms (includes compile for C, Rust, Go).\n\n", n)
	fmt.Println("| Language | Program | Cold p50 | Cold p95 | Warm p50 | Warm p95 | p50 saved |")
	fmt.Println("|---|---|---:|---:|---:|---:|---:|")
	ctx := context.Background()
	for _, name := range langOrder {
		p := pool.New(r, name, langs[name], 1, time.Minute)
		coldPool := pool.New(r, name, langs[name], 0, 0) // nothing idle, no wait: the real cold path
		for _, prog := range []struct {
			label string
			code  map[string]string
		}{{"hello world", hello}, {"1 s compute", compute}} {
			var cold, warm []time.Duration
			for range n {
				start := time.Now()
				res, err := coldPool.Execute(ctx, prog.code[name], "", 5*time.Second, 0)
				cold = append(cold, time.Since(start))
				check(name, res, err)
				if res.Warm {
					log.Fatalf("%s: cold run was warm", name)
				}
			}
			for range n {
				for p.Stats().Idle == 0 {
					time.Sleep(5 * time.Millisecond)
				}
				start := time.Now()
				res, err := p.Execute(ctx, prog.code[name], "", 5*time.Second, 0)
				warm = append(warm, time.Since(start))
				check(name, res, err)
				if !res.Warm {
					log.Fatalf("%s: warm run went cold", name)
				}
			}
			c50, w50 := pct(cold, 50), pct(warm, 50)
			fmt.Printf("| %s | %s | %d | %d | %d | %d | %d (%.0f%%) |\n", name, prog.label,
				c50, pct(cold, 95), w50, pct(warm, 95), c50-w50, 100*float64(c50-w50)/float64(c50))
		}
		p.Close()
		coldPool.Close()
	}
}

func check(lang string, res runner.Result, err error) {
	if err != nil || res.Status != "ok" {
		log.Fatalf("%s: %+v %v", lang, res, err)
	}
}

func pct(ds []time.Duration, p int) int64 {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[(len(s)-1)*p/100].Milliseconds()
}

// load runs workers concurrent clients against the HTTP API for d, with a mix
// of normal and hostile programs, then checks for internal errors, leaked
// containers and goroutine growth. It reports whether all checks passed.
func load(r *runner.Runner, langs map[string]runner.Lang, workers, maxRuns int, d time.Duration) bool {
	type job struct {
		lang, code string
		memoryMB   int
		timeoutMS  int
		want       string
	}
	var jobs []job
	for _, name := range langOrder {
		jobs = append(jobs,
			job{lang: name, code: hello[name], want: "ok"},
			job{lang: name, code: hello[name], want: "ok"},
			job{lang: name, code: hello[name], want: "ok"},
			job{lang: name, code: compute[name], want: "ok"},
		)
	}
	jobs = append(jobs,
		job{lang: "python", code: "while True: pass", timeoutMS: 500, want: "timeout"},
		job{lang: "python", code: "x=[]\nwhile True: x.append(bytearray(10<<20))", memoryMB: 64, want: "memory_exceeded"},
		job{lang: "c", code: "#include <stdio.h>\nint main(){for(;;)puts(\"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\");}", want: "runtime_error"},
		job{lang: "c", code: "#include <unistd.h>\nint main(){for(;;)fork();}", timeoutMS: 500, want: "timeout"},
		job{lang: "rust", code: "fn main(){ let x: i32 = \"s\"; }", want: "compile_error"},
	)

	pools := map[string]*pool.Pool{}
	for _, name := range langOrder {
		pools[name] = pool.New(r, name, langs[name], 2, time.Second)
	}
	srv := httptest.NewServer(api.Routes(pools, maxRuns))
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}

	waitFull := func() {
		for _, p := range pools {
			for p.Stats().Idle < 2 {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	waitFull()
	before := runtime.NumGoroutine()
	containersBefore, _ := r.Containers(context.Background())
	log.Printf("load: %d workers, %d run slots, for %v; %d goroutines, %d containers (idle pools)", workers, maxRuns, d, before, containersBefore)

	var total, wrong, internal, warm, busy atomic.Int64
	var mu sync.Mutex
	statuses := map[string]int{}
	var latencies []time.Duration
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				j := jobs[rand.IntN(len(jobs))]
				body, _ := json.Marshal(map[string]any{"language": j.lang, "code": j.code,
					"limits": map[string]int{"timeout_ms": j.timeoutMS, "memory_mb": j.memoryMB}})
				start := time.Now()
				resp, err := client.Post(srv.URL+"/v1/execute", "application/json", bytes.NewReader(body))
				if err != nil {
					log.Printf("request: %v", err)
					internal.Add(1)
					continue
				}
				var res api.ExecuteResponse
				json.NewDecoder(resp.Body).Decode(&res)
				resp.Body.Close()
				el := time.Since(start)

				total.Add(1)
				if resp.StatusCode == http.StatusServiceUnavailable {
					busy.Add(1) // controlled overload rejection, not a failure
					mu.Lock()
					statuses["503 busy"]++
					mu.Unlock()
					continue
				}
				if res.Warm {
					warm.Add(1)
				}
				if res.Status == "internal_error" || resp.StatusCode != http.StatusOK {
					internal.Add(1)
				} else if res.Status != j.want {
					wrong.Add(1)
					log.Printf("%s: want %s, got %s (%s)", j.lang, j.want, res.Status, res.Stderr)
				}
				mu.Lock()
				statuses[res.Status]++
				latencies = append(latencies, el)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	// Let pools refill and background removals finish, and drop idle HTTP
	// connections (each holds goroutines on both ends), then compare.
	waitFull()
	client.CloseIdleConnections()
	srv.CloseClientConnections()
	time.Sleep(500 * time.Millisecond)
	after := runtime.NumGoroutine()
	containersAfter, _ := r.Containers(context.Background())

	srv.Close()
	for _, p := range pools {
		p.Close()
	}
	containersClosed, _ := r.Containers(context.Background())

	keys := make([]string, 0, len(statuses))
	for k := range statuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("requests: %d in %v (%.1f/s), warm: %d (%.0f%%)\n", total.Load(), d,
		float64(total.Load())/d.Seconds(), warm.Load(), 100*float64(warm.Load())/float64(total.Load()))
	for _, k := range keys {
		fmt.Printf("  %-16s %d\n", k, statuses[k])
	}
	fmt.Printf("latency ms: p50 %d, p95 %d, p99 %d\n", pct(latencies, 50), pct(latencies, 95), pct(latencies, 99))
	fmt.Printf("internal errors: %d\nwrong status: %d\nrejected busy (503): %d\n", internal.Load(), wrong.Load(), busy.Load())
	fmt.Printf("goroutines: %d before, %d after\n", before, after)
	fmt.Printf("containers: %d before, %d after (pools full), %d after Close\n", containersBefore, containersAfter, containersClosed)

	ok := internal.Load() == 0 && wrong.Load() == 0 && after <= before &&
		containersAfter == containersBefore && containersClosed == 0
	fmt.Println(map[bool]string{true: "PASS", false: "FAIL"}[ok])
	return ok
}
