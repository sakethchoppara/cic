// Package dockerproxy is a filtering proxy for the Docker API socket. It lets
// through only the calls the runner makes, only on cic-labelled containers,
// and only with the sandbox settings the runner uses. Anything else gets 403.
//
// Without it, whoever holds the Docker socket is root on the host: one
// "Privileged: true" or bind mount of / away. With it, a compromised or buggy
// runner can still start containers, but only sandboxed ones.
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const maxBody = 1 << 20

// Policy is what containers the proxy allows. Build it from languages.yaml.
type Policy struct {
	Images      map[string]bool // image references allowed in create
	MaxMemoryMB int             // largest Memory a create or update may set
	MaxPids     int             // largest PidsLimit a create or update may set
}

type Proxy struct {
	policy   Policy
	upstream *http.Client // for label lookups
	rp       *httputil.ReverseProxy
	cic      sync.Map // container ID -> struct{}, verified to carry the cic label
}

// New proxies to the Docker API reachable through dial (normally the daemon's unix socket).
func New(dial func(ctx context.Context) (net.Conn, error), policy Policy) *Proxy {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
	return &Proxy{
		policy:   policy,
		upstream: &http.Client{Transport: tr},
		rp: &httputil.ReverseProxy{
			Rewrite:   func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker" },
			Transport: tr, // exec start upgrades to a raw stream; ReverseProxy handles the 101
		},
	}
}

var (
	versionPrefix = regexp.MustCompile(`^/v[0-9.]+/`)
	containerOp   = regexp.MustCompile(`^/containers/([0-9a-f]{12,64})(/(start|kill|update|exec))?$`)
	execOp        = regexp.MustCompile(`^/exec/[0-9a-f]{64}/(start|json)$`)
	imageInspect  = regexp.MustCompile(`^/images/[^/]+/json$`)
)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := p.check(r); err != nil {
		log.Printf("denied %s %s: %v", r.Method, r.URL.RequestURI(), err)
		deny(w, err)
		return
	}
	p.rp.ServeHTTP(w, r)
}

func (p *Proxy) check(r *http.Request) error {
	// The daemon may clean ".." or decode %2F, so a path that matches below could
	// mean something else to it. CIC never sends either.
	if strings.Contains(r.URL.Path, "..") || r.URL.RawPath != "" {
		return fmt.Errorf("path %q is not allowed", r.URL.EscapedPath())
	}
	path := versionPrefix.ReplaceAllString(r.URL.Path, "/")
	switch {
	case (r.Method == "GET" || r.Method == "HEAD") && (path == "/_ping" || path == "/version"):
		return nil
	case r.Method == "GET" && imageInspect.MatchString(path):
		return nil
	case r.Method == "GET" && path == "/containers/json":
		return checkListFilter(r.URL.Query().Get("filters"))
	case r.Method == "POST" && path == "/containers/create":
		return withBody(r, p.checkCreate)
	case r.Method == "POST" && execOp.MatchString(path), r.Method == "GET" && execOp.MatchString(path):
		// Exec IDs are unguessable and only come from exec creates, which are checked below.
		return nil
	}

	m := containerOp.FindStringSubmatch(path)
	if m == nil {
		return fmt.Errorf("%s %s is not allowed", r.Method, path)
	}
	id, op := m[1], m[3]
	switch {
	case r.Method == "DELETE" && op == "":
	case r.Method == "POST" && (op == "start" || op == "kill" || op == "update" || op == "exec"):
	default:
		return fmt.Errorf("%s %s is not allowed", r.Method, path)
	}
	if err := p.checkCIC(r.Context(), id); err != nil {
		return err
	}
	switch op {
	case "update":
		return withBody(r, p.checkUpdate)
	case "exec":
		return withBody(r, checkExec)
	case "":
		p.cic.Delete(id) // being removed; IDs are never reused
	}
	return nil
}

// checkCIC allows operations only on containers labelled cic, so the runner
// can't stop, exec into, or remove anything else on the host.
func (p *Proxy) checkCIC(ctx context.Context, id string) error {
	if _, ok := p.cic.Load(id); ok {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/"+id+"/json", nil)
	resp, err := p.upstream.Do(req)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", id, err)
	}
	defer resp.Body.Close()
	var c struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&c) != nil {
		return fmt.Errorf("container %s not found", id)
	}
	if _, ok := c.Config.Labels["cic"]; !ok {
		return fmt.Errorf("container %s is not a cic container", id)
	}
	p.cic.Store(id, struct{}{})
	p.cic.Store(c.ID, struct{}{}) // short and full IDs both resolve
	return nil
}

// checkListFilter requires container listings to be filtered to cic labels,
// so the runner can't enumerate the host's other containers.
func checkListFilter(raw string) error {
	var f map[string]map[string]bool
	if raw == "" || json.Unmarshal([]byte(raw), &f) != nil || len(f["label"]) == 0 {
		return fmt.Errorf("container list must filter on the cic label")
	}
	for l := range f["label"] {
		if l != "cic" && !strings.HasPrefix(l, "cic=") {
			return fmt.Errorf("container list may only filter on cic labels, not %q", l)
		}
	}
	return nil
}

// fields decodes a JSON object and rejects any key outside allowed whose value
// isn't zero. Allowlisting (not blocklisting) means a field added to the
// Docker API later is denied until someone decides it's safe.
func fields(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bad JSON: %w", err)
	}
	for k, v := range m {
		if !slices.Contains(allowed, k) && !isZero(v) {
			return nil, fmt.Errorf("%s is not allowed", k)
		}
	}
	return m, nil
}

// isZero reports whether v is null, false, 0, "", or an array/object of zero
// values, which is how the client sends fields it doesn't use (for example
// "RestartPolicy": {"Name": ""}). A nested empty object or array is not zero:
// in {"Volumes": {"/data": {}}} the key's presence is the whole meaning.
func isZero(v json.RawMessage) bool {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return false
	}
	var zero func(x any, nested bool) bool
	zero = func(x any, nested bool) bool {
		switch x := x.(type) {
		case nil:
			return true
		case bool:
			return !x
		case float64:
			return x == 0
		case string:
			return x == ""
		case []any:
			if nested && len(x) == 0 {
				return false
			}
			for _, e := range x {
				if !zero(e, true) {
					return false
				}
			}
			return true
		case map[string]any:
			if nested && len(x) == 0 {
				return false
			}
			for _, e := range x {
				if !zero(e, true) {
					return false
				}
			}
			return true
		}
		return false
	}
	return zero(x, false)
}

func (p *Proxy) checkCreate(body []byte) error {
	top, err := fields(body, "Image", "User", "Env", "Cmd", "WorkingDir", "Labels", "HostConfig")
	if err != nil {
		return err
	}
	var c struct {
		Image  string
		User   string
		Labels map[string]string
	}
	json.Unmarshal(body, &c)
	if !p.policy.Images[c.Image] {
		return fmt.Errorf("image %q is not allowed", c.Image)
	}
	if err := nonRoot(c.User); err != nil {
		return err
	}
	if _, ok := c.Labels["cic"]; !ok {
		return fmt.Errorf("containers must carry the cic label")
	}

	if _, err := fields(top["HostConfig"], "NetworkMode", "ReadonlyRootfs", "Tmpfs", "CapDrop", "SecurityOpt",
		"Memory", "MemorySwap", "NanoCpus", "PidsLimit"); err != nil {
		return fmt.Errorf("HostConfig: %w", err)
	}
	var hc struct {
		NetworkMode    string
		ReadonlyRootfs bool
		Tmpfs          map[string]string
		CapDrop        []string
		SecurityOpt    []string
		Memory         int64
		MemorySwap     int64
		NanoCpus       int64
		PidsLimit      *int64
	}
	json.Unmarshal(top["HostConfig"], &hc)
	switch {
	case hc.NetworkMode != "none":
		return fmt.Errorf("NetworkMode must be none")
	case !hc.ReadonlyRootfs:
		return fmt.Errorf("ReadonlyRootfs must be true")
	case !slices.Contains(hc.CapDrop, "ALL"):
		return fmt.Errorf("CapDrop must include ALL")
	case !slices.Equal(hc.SecurityOpt, []string{"no-new-privileges"}):
		return fmt.Errorf("SecurityOpt must be exactly [no-new-privileges]")
	case hc.NanoCpus <= 0 || hc.NanoCpus > 1e9:
		return fmt.Errorf("NanoCpus must be set, at most 1 CPU")
	}
	for path, opts := range hc.Tmpfs {
		if path != "/tmp" && path != "/work" {
			return fmt.Errorf("tmpfs at %s is not allowed", path)
		}
		if !strings.Contains(opts, "size=") {
			return fmt.Errorf("tmpfs at %s needs a size", path)
		}
	}
	return p.checkLimits(hc.Memory, hc.MemorySwap, hc.PidsLimit)
}

func (p *Proxy) checkUpdate(body []byte) error {
	if _, err := fields(body, "Memory", "MemorySwap", "PidsLimit"); err != nil {
		return err
	}
	var u struct {
		Memory, MemorySwap int64
		PidsLimit          *int64
	}
	json.Unmarshal(body, &u)
	return p.checkLimits(u.Memory, u.MemorySwap, u.PidsLimit)
}

func (p *Proxy) checkLimits(mem, swap int64, pids *int64) error {
	switch {
	case mem <= 0 || mem > int64(p.policy.MaxMemoryMB)<<20:
		return fmt.Errorf("Memory must be set, at most %d MB", p.policy.MaxMemoryMB)
	case swap != mem:
		return fmt.Errorf("MemorySwap must equal Memory (no swap)")
	case pids == nil || *pids <= 0 || *pids > int64(p.policy.MaxPids):
		return fmt.Errorf("PidsLimit must be set, at most %d", p.policy.MaxPids)
	}
	return nil
}

func checkExec(body []byte) error {
	if _, err := fields(body, "User", "AttachStdin", "AttachStdout", "AttachStderr", "Env", "WorkingDir", "Cmd"); err != nil {
		return err
	}
	var e struct{ User string }
	json.Unmarshal(body, &e)
	return nonRoot(e.User)
}

// nonRoot rejects an empty user (the image default, often root), root, and UID or GID 0.
func nonRoot(user string) error {
	if user == "" {
		return fmt.Errorf("User must be set")
	}
	for _, part := range strings.Split(user, ":") {
		if part == "root" || strings.TrimLeft(part, "0") == "" {
			return fmt.Errorf("User %q is root", user)
		}
	}
	return nil
}

// withBody reads the request body (capped), checks it, and restores it for proxying.
func withBody(r *http.Request, check func([]byte) error) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return check(body)
}

// deny answers in the daemon's error format, so the client reports
// "Error response from daemon: cic proxy: ...".
func deny(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{"message": "cic proxy: " + err.Error()})
}
