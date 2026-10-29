package api

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/sakethchoppara/cic/internal/runner"
)

const (
	maxCode  = 64 << 10
	maxStdin = 64 << 10
	// JSON escaping can inflate code/stdin up to 6x (\u00XX), so the body cap is generous;
	// the per-field checks below are the real limits.
	maxBody = 6 * (maxCode + maxStdin)

	defaultTimeoutMS = 5000
	maxTimeoutMS     = 15000
	minMemoryMB      = 32
	maxMemoryMB      = 512
)

type ExecuteRequest struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Stdin    string `json:"stdin"`
	Limits   struct {
		TimeoutMS int `json:"timeout_ms"`
		MemoryMB  int `json:"memory_mb"`
	} `json:"limits"`
}

type Timings struct {
	QueueMS   int64 `json:"queue_ms"`
	CompileMS int64 `json:"compile_ms"`
	RunMS     int64 `json:"run_ms"`
	TotalMS   int64 `json:"total_ms"`
}

type ExecuteResponse struct {
	Status          string  `json:"status"`
	Phase           string  `json:"phase,omitempty"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	ExitCode        int     `json:"exit_code"`
	StdoutTruncated bool    `json:"stdout_truncated"`
	StderrTruncated bool    `json:"stderr_truncated"`
	Timings         Timings `json:"timings"`
	Warm            bool    `json:"warm"`
}

func Routes(run *runner.Runner, langs map[string]runner.Lang) http.Handler {
	names := slices.Sorted(maps.Keys(langs))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/execute", func(w http.ResponseWriter, r *http.Request) { execute(w, r, run, langs) })
	mux.HandleFunc("GET /v1/languages", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, names) })
	return mux
}

func execute(w http.ResponseWriter, r *http.Request, run *runner.Runner, langs map[string]runner.Lang) {
	start := time.Now()
	var req ExecuteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	lang, ok := langs[req.Language]
	if req.Limits.TimeoutMS == 0 {
		req.Limits.TimeoutMS = defaultTimeoutMS
	}
	switch {
	case len(req.Code) > maxCode || len(req.Stdin) > maxStdin:
		httpError(w, http.StatusRequestEntityTooLarge, "code and stdin are limited to 64 KB each")
		return
	case !ok:
		httpError(w, http.StatusBadRequest, "unknown language: "+req.Language)
		return
	case req.Code == "":
		httpError(w, http.StatusBadRequest, "code is required")
		return
	case req.Limits.TimeoutMS < 0 || req.Limits.TimeoutMS > maxTimeoutMS:
		httpError(w, http.StatusBadRequest, "timeout_ms must be between 1 and 15000")
		return
	case req.Limits.MemoryMB != 0 && (req.Limits.MemoryMB < minMemoryMB || req.Limits.MemoryMB > maxMemoryMB):
		httpError(w, http.StatusBadRequest, "memory_mb must be between 32 and 512")
		return
	}

	// r.Context(): a client disconnect cancels the run and frees the container.
	res, err := run.Run(r.Context(), lang, req.Code, req.Stdin,
		time.Duration(req.Limits.TimeoutMS)*time.Millisecond, req.Limits.MemoryMB)
	if err != nil {
		log.Printf("execute %s: %v", req.Language, err)
		writeJSON(w, http.StatusInternalServerError, ExecuteResponse{Status: "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, ExecuteResponse{
		Status:          res.Status,
		Phase:           res.Phase,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		ExitCode:        res.ExitCode,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		Timings:         Timings{CompileMS: res.CompileMS, RunMS: res.RunMS, TotalMS: time.Since(start).Milliseconds()},
	})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
