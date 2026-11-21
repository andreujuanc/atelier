package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// daemon is a fake Docker daemon on a unix socket. It knows a few objects by
// name and records what reaches it.
type daemon struct {
	mu       sync.Mutex
	requests []string          // "METHOD path?query"
	bodies   map[string]string // last body per "METHOD path"
	labels   map[string]map[string]string
}

const testID = "task-abc123"

func newDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	d := &daemon{bodies: map[string]string{}, labels: map[string]map[string]string{
		"containers/mine":  {LabelTask: testID},
		"containers/agent": {LabelAgent: testID},
		"containers/other": {LabelTask: "someone-else"},
		"containers/host":  {},
		"networks/mynet":   {LabelTask: testID},
		"networks/hostnet": {},
		"volumes/myvol":    {LabelTask: testID},
		"volumes/hostvol":  {},
	}}
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return d, sock
}

func (d *daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	uri := r.URL.Path
	if r.URL.RawQuery != "" {
		q, _ := url.QueryUnescape(r.URL.RawQuery)
		uri += "?" + q
	}
	d.requests = append(d.requests, r.Method+" "+uri)
	d.bodies[r.Method+" "+r.URL.Path] = string(body)
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(versionPrefix.ReplaceAllString(r.URL.Path, ""), "/"), "/")
	inspect := r.Method == "GET" && (len(parts) == 2 && parts[0] != "containers" || len(parts) == 3 && parts[0] == "containers" && parts[2] == "json")
	if inspect && parts[1] != "json" {
		labels, ok := d.labels[parts[0]+"/"+parts[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such object"}`)
			return
		}
		obj := map[string]any{"Labels": labels, "Config": map[string]any{"Labels": labels}}
		if parts[0] == "networks" {
			obj["Containers"] = map[string]any{"c1": map[string]any{"Name": "atl-" + testID}, "c2": map[string]any{"Name": "db"}}
		}
		json.NewEncoder(w).Encode(obj)
		return
	}
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/create") || strings.HasSuffix(r.URL.Path, "/exec") {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"new1"}`)
		return
	}
	io.WriteString(w, `{}`)
}

func (d *daemon) saw(prefix string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.requests) - 1; i >= 0; i-- {
		if strings.HasPrefix(d.requests[i], prefix) {
			return d.requests[i]
		}
	}
	return ""
}

func (d *daemon) body(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bodies[key]
}

type gateClient struct {
	t    *testing.T
	http *http.Client
	root string
	d    *daemon
}

// newGate serves a Gate for task testID in front of a fake daemon.
func newGate(t *testing.T) *gateClient {
	t.Helper()
	d, upstream := newDaemon(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := &Gate{ID: testID, Root: root, SocketHost: "/host/run/docker.sock", Agent: "atl-" + testID, Upstream: upstream}
	sock := filepath.Join(t.TempDir(), "gate.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: g}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	return &gateClient{t: t, http: client, root: root, d: d}
}

// do sends a request through the gate and returns the status and body.
func (c *gateClient) do(method, uri, body string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, "http://gate"+uri, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestGateRefusesHostReachingContainers(t *testing.T) {
	c := newGate(t)
	outside := t.TempDir()
	if err := os.Symlink("/", filepath.Join(c.root, "rootlink")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"host root bind":       `{"HostConfig":{"Binds":["/:/host"]}}`,
		"bind outside":         `{"HostConfig":{"Binds":["` + outside + `:/x:ro"]}}`,
		"bind via dotdot":      `{"HostConfig":{"Binds":["` + c.root + `/../x:/x"]}}`,
		"bind via symlink":     `{"HostConfig":{"Binds":["` + c.root + `/rootlink/etc:/x"]}}`,
		"shared propagation":   `{"HostConfig":{"Binds":["` + c.root + `:/x:rshared"]}}`,
		"mount outside":        `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/etc","Target":"/e"}]}}`,
		"mount propagation":    `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + c.root + `","Target":"/e","BindOptions":{"Propagation":"shared"}}]}}`,
		"volume driver opts":   `{"HostConfig":{"Mounts":[{"Type":"volume","Source":"myvol","Target":"/e","VolumeOptions":{"DriverConfig":{"Options":{"device":"/"}}}}]}}`,
		"another's volume":     `{"HostConfig":{"Binds":["hostvol:/data"]}}`,
		"privileged":           `{"HostConfig":{"Privileged":true}}`,
		"host pid":             `{"HostConfig":{"PidMode":"host"}}`,
		"host network":         `{"HostConfig":{"NetworkMode":"host"}}`,
		"host ipc":             `{"HostConfig":{"IpcMode":"host"}}`,
		"host userns":          `{"HostConfig":{"UsernsMode":"host"}}`,
		"host cgroupns":        `{"HostConfig":{"CgroupnsMode":"host"}}`,
		"cap add":              `{"HostConfig":{"CapAdd":["SYS_ADMIN"]}}`,
		"device":               `{"HostConfig":{"Devices":[{"PathOnHost":"/dev/kvm"}]}}`,
		"security opt":         `{"HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`,
		"unmasked paths":       `{"HostConfig":{"MaskedPaths":[]}}`,
		"runtime":              `{"HostConfig":{"Runtime":"other"}}`,
		"sysctls":              `{"HostConfig":{"Sysctls":{"kernel.x":"1"}}}`,
		"unknown setting":      `{"HostConfig":{"FutureHostThing":"x"}}`,
		"another's network":    `{"HostConfig":{"NetworkMode":"hostnet"}}`,
		"another's endpoint":   `{"NetworkingConfig":{"EndpointsConfig":{"hostnet":{}}}}`,
		"another's netns":      `{"HostConfig":{"NetworkMode":"container:other"}}`,
		"another's volumes":    `{"HostConfig":{"VolumesFrom":["other"]}}`,
		"own container's vols": `{"HostConfig":{"VolumesFrom":["agent"]}}`,
	} {
		status, out := c.do("POST", "/v1.45/containers/create", body)
		if status != http.StatusForbidden || !strings.Contains(out, "atelier gate:") {
			t.Errorf("%s: status %d, body %s; want a refusal", name, status, out)
		}
	}
	if got := c.d.saw("POST /v1.45/containers/create"); got != "" {
		t.Errorf("a refused create reached the daemon: %s", got)
	}
}

func TestGateForwardsTaskContainers(t *testing.T) {
	c := newGate(t)
	work := filepath.Join(c.root, "repo")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"Image":"postgres","Labels":{"mine":"1","atelier.agent":"` + testID + `"},"HostConfig":{
		"Binds":["` + work + `:/app:ro","` + c.root + `/new/dir:/n","myvol:/data","/var/run/docker.sock:/var/run/docker.sock"],
		"Mounts":[{"Type":"bind","Source":"` + work + `","Target":"/m"},{"Type":"tmpfs","Target":"/t"}],
		"NetworkMode":"mynet","Memory":9007199254740993,"MemorySwappiness":-1,"Privileged":false,"CapAdd":[],
		"PortBindings":{"5432/tcp":[{"HostPort":"5732"}]},"RestartPolicy":{"Name":"no"},"MaskedPaths":null},
		"NetworkingConfig":{"EndpointsConfig":{"mynet":{}}}}`
	status, out := c.do("POST", "/v1.45/containers/create?name=db", body)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d: %s", status, out)
	}
	var got struct {
		Labels     map[string]string
		HostConfig struct {
			Binds  []string
			Memory json.Number
		}
	}
	if err := json.Unmarshal([]byte(c.d.body("POST /v1.45/containers/create")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[LabelTask] != testID || got.Labels["mine"] != "1" {
		t.Errorf("labels = %v, want the task label added and others kept", got.Labels)
	}
	if _, ok := got.Labels[LabelAgent]; ok {
		t.Error("a task could label a container as its own container")
	}
	want := []string{work + ":/app:ro", c.root + "/new/dir:/n", "myvol:/data", "/host/run/docker.sock:/var/run/docker.sock"}
	if strings.Join(got.HostConfig.Binds, " ") != strings.Join(want, " ") {
		t.Errorf("binds = %q, want %q (the Docker socket becomes the gate's)", got.HostConfig.Binds, want)
	}
	if got.HostConfig.Memory.String() != "9007199254740993" {
		t.Errorf("a 64-bit number was changed on the way: %s", got.HostConfig.Memory)
	}

	// A volume named in a mount that does not exist yet is created with the label.
	if status, out := c.do("POST", "/containers/create", `{"HostConfig":{"Binds":["fresh:/data"]}}`); status != http.StatusCreated {
		t.Fatalf("create with a new volume: %d %s", status, out)
	}
	if b := c.d.body("POST /volumes/create"); !strings.Contains(b, `"atelier.task":"`+testID+`"`) || !strings.Contains(b, `"fresh"`) {
		t.Errorf("volume create body = %s", b)
	}
}

func TestGateOwnership(t *testing.T) {
	c := newGate(t)
	for _, tc := range []struct {
		method, uri string
		want        int
	}{
		{"GET", "/containers/mine/json", 200},
		{"POST", "/containers/mine/stop", 200},
		{"DELETE", "/containers/mine", 200},
		{"GET", "/containers/missing/json", 404}, // the daemon's own answer
		{"GET", "/containers/other/json", 403},
		{"GET", "/containers/host/json", 403},
		{"POST", "/containers/host/stop", 403},
		{"DELETE", "/containers/other", 403},
		{"POST", "/containers/other/exec", 403},
		// The task's own container: look and exec, but not stop or remove.
		{"GET", "/containers/agent/json", 200},
		{"GET", "/containers/agent/archive", 200},
		{"POST", "/containers/agent/exec", 201},
		{"POST", "/containers/agent/stop", 403},
		{"POST", "/containers/agent/kill", 403},
		{"POST", "/containers/agent/rename", 403},
		{"DELETE", "/containers/agent", 403},
		{"GET", "/networks/mynet", 200},
		{"DELETE", "/networks/mynet", 200},
		{"GET", "/networks/hostnet", 403},
		{"DELETE", "/networks/hostnet", 403},
		{"GET", "/volumes/myvol", 200},
		{"DELETE", "/volumes/hostvol", 403},
		{"GET", "/exec/unknown/json", 403},
		{"POST", "/exec/unknown/start", 403},
	} {
		if status, out := c.do(tc.method, "/v1.45"+tc.uri, "{}"); status != tc.want {
			t.Errorf("%s %s: status %d, want %d (%s)", tc.method, tc.uri, status, tc.want, out)
		}
	}
	// An exec the task created is its own.
	if status, _ := c.do("GET", "/exec/new1/json", ""); status != 200 {
		t.Errorf("exec created through the gate: status %d", status)
	}
	if status, out := c.do("POST", "/containers/mine/exec", `{"Privileged":true}`); status != 403 {
		t.Errorf("privileged exec: %d %s", status, out)
	}
	// Networks: connect only the task's containers (its own one included).
	for body, want := range map[string]int{`{"Container":"mine"}`: 200, `{"Container":"agent"}`: 200, `{"Container":"host"}`: 403, `{}`: 403} {
		if status, out := c.do("POST", "/networks/mynet/connect", body); status != want {
			t.Errorf("connect %s: status %d, want %d (%s)", body, status, want, out)
		}
	}
	if status, _ := c.do("POST", "/networks/hostnet/connect", `{"Container":"mine"}`); status != 403 {
		t.Errorf("connect to another's network: %d", status)
	}
	// The gate hides the task's own container in a network it joined it to.
	if _, out := c.do("GET", "/networks/mynet", ""); strings.Contains(out, "atl-"+testID) || !strings.Contains(out, `"db"`) {
		t.Errorf("network inspect = %s", out)
	}
}

func TestGateListsOnlyTheTask(t *testing.T) {
	c := newGate(t)
	own := LabelTask + "=" + testID
	for uri, want := range map[string]string{
		"/containers/json?all=1": `{"label":{"` + own + `":true}}`,
		"/networks?filters=" + url.QueryEscape(`{"name":{"x_default":true}}`):            `"label":{"` + own + `":true}`,
		"/volumes?filters=" + url.QueryEscape(`{"label":{"com.docker.compose=x":true}}`): `"` + own + `":true`,
		"/events?filters=" + url.QueryEscape(`{"type":["container"]}`):                   `"label":["` + own + `"]`,
		"/containers/json?filters=" + url.QueryEscape(`{"label":["a=b"]}`):               `"label":["a=b","` + own + `"]`,
	} {
		if status, out := c.do("GET", uri, ""); status != 200 {
			t.Fatalf("%s: %d %s", uri, status, out)
		}
		path, _, _ := strings.Cut(uri, "?")
		if got := c.d.saw("GET " + path); !strings.Contains(got, want) {
			t.Errorf("%s reached the daemon as %s, want filters with %s", uri, got, want)
		}
	}
}

func TestGateRefusesEverythingElse(t *testing.T) {
	c := newGate(t)
	for _, tc := range []struct{ method, uri, body string }{
		{"POST", "/containers/prune", ""},
		{"POST", "/volumes/prune", ""},
		{"DELETE", "/images/postgres", ""},
		{"POST", "/images/load", ""},
		{"POST", "/images/create?fromSrc=-", ""},
		{"POST", "/commit?container=mine", ""},
		{"GET", "/system/df", ""},
		{"POST", "/swarm/init", ""},
		{"POST", "/session", ""},
		{"POST", "/build?version=2", ""},
		{"POST", "/build?networkmode=host", ""},
		{"POST", "/containers/mine/update", "{}"},
		{"GET", "/containers/mine/export", ""},
		{"GET", "/containers/mine%2Fx/json", ""},
		{"POST", "/volumes/create", `{"Name":"evil","DriverOpts":{"type":"none","o":"bind","device":"/"}}`},
		{"POST", "/volumes/create", `{"Name":"evil","Driver":"nfs"}`},
		{"POST", "/networks/create", `{"Name":"n","Driver":"host"}`},
		{"POST", "/networks/create", `{"Name":"n","Options":{"com.docker.network.bridge.name":"docker0"}}`},
		{"POST", "/networks/create", `{"Name":"n","ConfigFrom":{"Network":"hostnet"}}`},
	} {
		before := len(c.d.requests)
		if status, out := c.do(tc.method, tc.uri, tc.body); status != http.StatusForbidden {
			t.Errorf("%s %s %s: status %d (%s), want 403", tc.method, tc.uri, tc.body, status, out)
		}
		for _, r := range c.d.requests[before:] {
			if !strings.HasPrefix(r, "GET ") {
				t.Errorf("%s %s: %s reached the daemon", tc.method, tc.uri, r)
			}
		}
	}
	// What a task needs still passes.
	for _, tc := range []struct{ method, uri, body string }{
		{"GET", "/_ping", ""},
		{"GET", "/v1.45/version", ""},
		{"GET", "/images/localstack/localstack:4/json", ""},
		{"POST", "/images/create?fromImage=postgres&tag=17", ""},
		{"POST", "/build?networkmode=default", ""},
		{"POST", "/volumes/create", `{"Name":"data"}`},
		{"POST", "/networks/create", `{"Name":"n","Driver":"bridge","IPAM":{"Config":[{"Subnet":"10.1.0.0/24"}]}}`},
	} {
		if status, out := c.do(tc.method, tc.uri, tc.body); status >= 400 {
			t.Errorf("%s %s: status %d (%s), want it forwarded", tc.method, tc.uri, status, out)
		}
	}
	// A network the task creates is labelled, and its own container joins it.
	if b := c.d.body("POST /networks/create"); !strings.Contains(b, `"atelier.task":"`+testID+`"`) {
		t.Errorf("network create body = %s", b)
	}
	if b := c.d.body("POST /networks/new1/connect"); !strings.Contains(b, `"atl-`+testID+`"`) {
		t.Errorf("the task's own container was not joined to its new network: %q", b)
	}
}

func TestServeOneGatePerRunDir(t *testing.T) {
	_, upstream := newDaemon(t)
	runDir := filepath.Join(t.TempDir(), "run")
	root := t.TempDir()
	g := &Gate{ID: testID, Root: root, Upstream: upstream}
	if Alive(runDir) {
		t.Fatal("alive before it started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, g, &Control{Slug: "t1", TaskID: 1, Atl: "/bin/false"}, runDir) }()
	deadline := time.Now().Add(5 * time.Second)
	for !Alive(runDir) {
		if time.Now().After(deadline) {
			t.Fatal("gate never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A second gate for the same run dir steps aside and leaves the socket alone.
	if err := Serve(context.Background(), &Gate{ID: testID, Root: root, Upstream: upstream}, nil, runDir); err != nil {
		t.Fatalf("second Serve: %v", err)
	}
	if !Alive(runDir) {
		t.Fatal("the second gate took the first one's socket down")
	}
	if code, err := ControlClient(ControlSocketPath(runDir), ControlRequest{Op: OpFetch}, io.Discard); err != nil || code != 1 {
		t.Errorf("control socket of the first gate: exit %d, %v; want /bin/false's 1", code, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	for _, sock := range []string{SocketPath(runDir), ControlSocketPath(runDir)} {
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("socket %s left behind: %v", sock, err)
		}
	}
}
