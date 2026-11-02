package dockerproxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	cicID   = "1111111111111111111111111111111111111111111111111111111111111111"
	otherID = "2222222222222222222222222222222222222222222222222222222222222222"
	execID  = "3333333333333333333333333333333333333333333333333333333333333333"
)

// A create body as the runner sends it (captured from a real run), as a map so tests can mutate it.
func goodCreate() map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(`{
		"User": "nobody", "Env": ["HOME=/tmp"], "Cmd": ["sleep", "infinity"], "Image": "python:3.12-slim",
		"WorkingDir": "/work", "Labels": {"cic": "123-abc"}, "Entrypoint": null, "Volumes": null,
		"HostConfig": {
			"LogConfig": {"Type": "", "Config": null}, "NetworkMode": "none",
			"RestartPolicy": {"Name": "", "MaximumRetryCount": 0}, "ConsoleSize": [0, 0],
			"CapDrop": ["ALL"], "CapAdd": null, "Privileged": false, "Binds": null, "PidMode": "",
			"ReadonlyRootfs": true, "SecurityOpt": ["no-new-privileges"],
			"Tmpfs": {"/tmp": "rw,size=16m,noexec", "/work": "rw,size=16m,exec"},
			"Memory": 268435456, "MemorySwap": 268435456, "NanoCpus": 1000000000, "PidsLimit": 64
		}}`), &m)
	return m
}

func newTestProxy(t *testing.T) http.Handler {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/" + cicID + "/json":
			w.Write([]byte(`{"Id":"` + cicID + `","Config":{"Labels":{"cic":"x"}}}`))
		case "/containers/" + otherID + "/json":
			w.Write([]byte(`{"Id":"` + otherID + `","Config":{"Labels":{"app":"db"}}}`))
		default:
			w.Write([]byte(`{"forwarded":true}`))
		}
	}))
	t.Cleanup(daemon.Close)
	dial := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", daemon.Listener.Addr().String())
	}
	return New(dial, Policy{Images: map[string]bool{"python:3.12-slim": true}, MaxMemoryMB: 1024, MaxPids: 256})
}

func do(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var b string
	switch v := body.(type) {
	case string:
		b = v
	case nil:
	default:
		raw, _ := json.Marshal(v)
		b = string(raw)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "http://docker"+path, strings.NewReader(b)))
	return rec
}

func TestAllowed(t *testing.T) {
	h := newTestProxy(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"HEAD", "/_ping", nil},
		{"GET", "/v1.56/version", nil},
		{"GET", "/v1.56/images/python:3.12-slim/json", nil},
		{"GET", `/v1.56/containers/json?all=1&filters={"label":{"cic=123-abc":true}}`, nil},
		{"POST", "/v1.56/containers/create", goodCreate()},
		{"POST", "/v1.56/containers/" + cicID + "/start", nil},
		{"POST", "/v1.56/containers/" + cicID + "/kill?signal=KILL", nil},
		{"POST", "/v1.56/containers/" + cicID + "/update", `{"Memory":268435456,"MemorySwap":268435456,"PidsLimit":64,"NanoCpus":0,"RestartPolicy":{"Name":""}}`},
		{"POST", "/v1.56/containers/" + cicID + "/exec", `{"User":"nobody","Privileged":false,"AttachStdin":true,"AttachStdout":true,"Cmd":["cat"]}`},
		{"POST", "/v1.56/exec/" + execID + "/start", `{"Detach":false}`},
		{"GET", "/v1.56/exec/" + execID + "/json", nil},
		{"DELETE", "/v1.56/containers/" + cicID + "?force=1", nil},
	} {
		if rec := do(h, c.method, c.path, c.body); rec.Code != 200 {
			t.Errorf("%s %s: got %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

func TestDeniedCreate(t *testing.T) {
	h := newTestProxy(t)
	hostConfig := func(k string, v any) func(map[string]any) {
		return func(m map[string]any) { m["HostConfig"].(map[string]any)[k] = v }
	}
	top := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	for name, mutate := range map[string]func(map[string]any){
		"privileged":              hostConfig("Privileged", true),
		"privileged, lowercase":   hostConfig("privileged", true),
		"bind mount":              hostConfig("Binds", []string{"/:/host"}),
		"mount":                   hostConfig("Mounts", []map[string]any{{"Type": "bind", "Source": "/", "Target": "/host"}}),
		"docker socket volume":    hostConfig("Binds", []string{"/var/run/docker.sock:/var/run/docker.sock"}),
		"cap add":                 hostConfig("CapAdd", []string{"SYS_ADMIN"}),
		"no cap drop":             hostConfig("CapDrop", nil),
		"host network":            hostConfig("NetworkMode", "host"),
		"bridge network":          hostConfig("NetworkMode", "bridge"),
		"host pid":                hostConfig("PidMode", "host"),
		"host ipc":                hostConfig("IpcMode", "host"),
		"host userns":             hostConfig("UsernsMode", "host"),
		"host cgroupns":           hostConfig("CgroupnsMode", "host"),
		"device":                  hostConfig("Devices", []map[string]string{{"PathOnHost": "/dev/sda"}}),
		"writable rootfs":         hostConfig("ReadonlyRootfs", false),
		"seccomp unconfined":      hostConfig("SecurityOpt", []string{"no-new-privileges", "seccomp=unconfined"}),
		"no no-new-privileges":    hostConfig("SecurityOpt", nil),
		"sysctl":                  hostConfig("Sysctls", map[string]string{"kernel.core_pattern": "|/x"}),
		"runtime":                 hostConfig("Runtime", "runc"),
		"oom kill disabled":       hostConfig("OomKillDisable", true),
		"no memory limit":         hostConfig("Memory", 0),
		"memory too large":        hostConfig("Memory", 2048<<20),
		"swap":                    hostConfig("MemorySwap", -1),
		"no pids limit":           hostConfig("PidsLimit", nil),
		"pids too large":          hostConfig("PidsLimit", 100000),
		"no cpu limit":            hostConfig("NanoCpus", 0),
		"tmpfs over /etc":         hostConfig("Tmpfs", map[string]string{"/etc": "size=1m"}),
		"tmpfs without size":      hostConfig("Tmpfs", map[string]string{"/work": "rw,exec"}),
		"port binding":            hostConfig("PortBindings", map[string]any{"22/tcp": []map[string]string{{"HostPort": "22"}}}),
		"no host config":          top("HostConfig", nil),
		"image not allowed":       top("Image", "ubuntu:latest"),
		"root user":               top("User", "root"),
		"uid 0":                   top("User", "0"),
		"gid 0":                   top("User", "nobody:0"),
		"default user":            top("User", ""),
		"no cic label":            top("Labels", map[string]string{"app": "x"}),
		"anonymous volume":        top("Volumes", map[string]any{"/data": map[string]any{}}),
		"entrypoint":              top("Entrypoint", []string{"/bin/sh"}),
		"host config, lowercased": top("hostconfig", map[string]any{"Privileged": true}),
	} {
		m := goodCreate()
		mutate(m)
		if rec := do(h, "POST", "/v1.56/containers/create", m); rec.Code != 403 || !strings.Contains(rec.Body.String(), "cic proxy") {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestDeniedOther(t *testing.T) {
	h := newTestProxy(t)
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"exec into a non-cic container", "POST", "/v1.56/containers/" + otherID + "/exec", `{"User":"nobody","Cmd":["sh"]}`},
		{"kill a non-cic container", "POST", "/v1.56/containers/" + otherID + "/kill", nil},
		{"remove a non-cic container", "DELETE", "/v1.56/containers/" + otherID, nil},
		{"unknown container", "POST", "/v1.56/containers/" + strings.Repeat("9", 64) + "/start", nil},
		{"privileged exec", "POST", "/v1.56/containers/" + cicID + "/exec", `{"User":"nobody","Privileged":true,"Cmd":["sh"]}`},
		{"root exec", "POST", "/v1.56/containers/" + cicID + "/exec", `{"User":"root","Cmd":["sh"]}`},
		{"exec without user", "POST", "/v1.56/containers/" + cicID + "/exec", `{"Cmd":["sh"]}`},
		{"update raises memory past policy", "POST", "/v1.56/containers/" + cicID + "/update", `{"Memory":4294967296,"MemorySwap":4294967296,"PidsLimit":64}`},
		{"update adds CPUs", "POST", "/v1.56/containers/" + cicID + "/update", `{"Memory":268435456,"MemorySwap":268435456,"PidsLimit":64,"NanoCpus":8000000000}`},
		{"update removes pids limit", "POST", "/v1.56/containers/" + cicID + "/update", `{"Memory":268435456,"MemorySwap":268435456,"PidsLimit":-1}`},
		{"copy files in", "PUT", "/v1.56/containers/" + cicID + "/archive?path=/", nil},
		{"attach", "POST", "/v1.56/containers/" + cicID + "/attach", nil},
		{"commit", "POST", "/v1.56/commit?container=" + cicID, nil},
		{"list all containers", "GET", "/v1.56/containers/json?all=1", nil},
		{"list with other filter", "GET", `/v1.56/containers/json?filters={"label":{"app":true}}`, nil},
		{"pull image", "POST", "/v1.56/images/create?fromImage=ubuntu", nil},
		{"build", "POST", "/v1.56/build", nil},
		{"volumes", "GET", "/v1.56/volumes", nil},
		{"networks", "POST", "/v1.56/networks/create", `{}`},
		{"swarm", "POST", "/v1.56/swarm/init", `{}`},
		{"plugins", "POST", "/v1.56/plugins/pull", nil},
		{"dot-dot path", "POST", "/v1.56/containers/" + cicID + "/start/../../../build", nil},
		{"escaped slash", "GET", "/v1.56/images/x%2F..%2F..%2Fvolumes/json", nil},
	} {
		if rec := do(h, c.method, c.path, c.body); rec.Code != 403 {
			t.Errorf("%s: got %d %s", c.name, rec.Code, rec.Body)
		}
	}
}
