package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

const (
	maxCode  = 64 << 10
	maxStdin = 64 << 10
	// JSON escaping can inflate code/stdin up to 6x (\u00XX), so the body cap is generous;
	// the per-field checks below are the real limits.
	maxBody = 6 * (maxCode + maxStdin)
)

// ponytail: hardcoded until config/languages.yaml lands in Week 3.
var languages = map[string]bool{"python": true, "node": true, "c": true, "rust": true, "go": true}

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

func Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/execute", execute)
	return mux
}

func execute(w http.ResponseWriter, r *http.Request) {
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
	switch {
	case len(req.Code) > maxCode || len(req.Stdin) > maxStdin:
		httpError(w, http.StatusRequestEntityTooLarge, "code and stdin are limited to 64 KB each")
	case !languages[req.Language]:
		httpError(w, http.StatusBadRequest, "unknown language: "+req.Language)
	case req.Code == "":
		httpError(w, http.StatusBadRequest, "code is required")
	default:
		// Stub: Week 2 replaces this with the runner.
		writeJSON(w, http.StatusOK, ExecuteResponse{Status: "ok"})
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
