package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sakethchoppara/cic/internal/pool"
)

func TestExecute(t *testing.T) {
	big := strings.Repeat("a", maxCode+1)
	cases := []struct {
		name, method, body string
		want               int
	}{
		{"timeout too large", "POST", `{"language":"python","code":"x","limits":{"timeout_ms":15001}}`, 400},
		{"memory too large", "POST", `{"language":"python","code":"x","limits":{"memory_mb":513}}`, 400},
		{"unknown language", "POST", `{"language":"cobol","code":"x"}`, 400},
		{"missing code", "POST", `{"language":"python"}`, 400},
		{"bad json", "POST", `{`, 400},
		{"unknown field", "POST", `{"language":"python","code":"x","shell":true}`, 400},
		{"code too large", "POST", `{"language":"python","code":"` + big + `"}`, 413},
		{"body too large", "POST", `{"language":"python","code":"` + strings.Repeat("a", maxBody) + `"}`, 413},
		{"wrong method", "GET", ``, 405},
	}
	h := Routes(map[string]*pool.Pool{"python": nil}, 1) // every case is rejected before the runner is touched; runner has its own tests
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, "/v1/execute", strings.NewReader(c.body)))
			if rec.Code != c.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
}

func TestBusy(t *testing.T) {
	slotWait = 10 * time.Millisecond
	t.Cleanup(func() { slotWait = 10 * time.Second })
	h := Routes(map[string]*pool.Pool{"python": nil}, 0) // no slots: every valid request waits, then 503
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{"language":"python","code":"x"}`)))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
}
