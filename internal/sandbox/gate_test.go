package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// daemon is a fake Docker daemon on a unix socket. It knows a few objects,
// finds them as dockerd does (full ID, then name, then unique ID prefix;
// volumes by name only) and records what reaches it.
type daemon struct {
	mu       sync.Mutex
	requests []string          // "METHOD path?query"
	bodies   map[string]string // last body per "METHOD path"
	objects  []*object
	execIn   []string // IDs of the containers execs were created in
	// onInspect, if set, runs before an inspect of an object is answered.
	onInspect func(kind, ref string)
	// volumesMissing are the volumes a container create named by name that
	// did not exist when the create reached the daemon (which would create
	// them itself, without the task's label).
	volumesMissing []string
}

type object struct {
	kind, id, name string
	labels         map[string]string
}

const testID = "task-abc123"

// IDs of the fake daemon's objects.
const (
	mineID    = "1111111111111111111111111111111111111111111111111111111111111111"
	agentID   = "2222222222222222222222222222222222222222222222222222222222222222"
	otherID   = "3333333333333333333333333333333333333333333333333333333333333333"
	hostID    = "abcd999999999999999999999999999999999999999999999999999999999999" // the box, say
	mynetID   = "4444444444444444444444444444444444444444444444444444444444444444"
	hostnetID = "abcd555555555555555555555555555555555555555555555555555555555555"
)

func newDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	d := &daemon{bodies: map[string]string{}, objects: []*object{
		{"containers", mineID, "mine", map[string]string{LabelTask: testID}},
		{"containers", agentID, "agent", map[string]string{LabelAgent: testID}},
		{"containers", otherID, "other", map[string]string{LabelTask: "someone-else"}},
		{"containers", hostID, "host", map[string]string{}},
		{"networks", mynetID, "mynet", map[string]string{LabelTask: testID}},
		{"networks", hostnetID, "hostnet", map[string]string{}},
		{"volumes", "myvol", "myvol", map[string]string{LabelTask: testID}},
		{"volumes", "hostvol", "hostvol", map[string]string{}},
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
	var obj *object
	if len(parts) >= 2 && oneOf(parts[0], "containers", "networks", "volumes") && !oneOf(parts[1], "json", "create", "prune") {
		if obj = d.find(parts[0], parts[1]); obj == nil {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such object"}`)
			return
		}
	}
	inspect := r.Method == "GET" && (len(parts) == 2 && parts[0] != "containers" || len(parts) == 3 && parts[0] == "containers" && parts[2] == "json")
	d.mu.Lock()
	onInspect := d.onInspect
	d.mu.Unlock()
	if inspect && obj != nil && onInspect != nil {
		onInspect(parts[0], parts[1])
	}
	if r.Method == "POST" && len(parts) == 2 && parts[0] == "containers" && parts[1] == "create" {
		d.noteMissingVolumes(body)
	}
	switch {
	case inspect && obj != nil:
		out := map[string]any{"Labels": obj.labels, "Config": map[string]any{"Labels": obj.labels}, "Name": obj.name}
		if obj.kind != "volumes" {
			out["Id"] = obj.id
		}
		if obj.kind == "containers" {
			out["Name"] = "/" + obj.name
		}
		if obj.kind == "networks" {
			out["Containers"] = map[string]any{"c1": map[string]any{"Name": "agent"}, "c2": map[string]any{"Name": "db"}}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && len(parts) == 3 && parts[2] == "rename" && obj != nil:
		d.mu.Lock()
		obj.name = r.URL.Query().Get("name")
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "DELETE" && len(parts) == 2 && obj != nil:
		d.mu.Lock()
		for i, o := range d.objects {
			if o == obj {
				d.objects = append(d.objects[:i], d.objects[i+1:]...)
				break
			}
		}
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "POST" && len(parts) == 3 && parts[2] == "exec" && obj != nil:
		d.mu.Lock()
		d.execIn = append(d.execIn, obj.id)
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"new1"}`)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/create"):
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"new1"}`)
	default:
		io.WriteString(w, `{}`)
	}
}

// noteMissingVolumes records the volumes a container create names (in
// Binds or Mounts) that do not exist.
func (d *daemon) noteMissingVolumes(body []byte) {
	var create struct {
		HostConfig struct {
			Binds  []string
			Mounts []struct{ Type, Source string }
		}
	}
	if json.Unmarshal(body, &create) != nil {
		return
	}
	var names []string
	for _, b := range create.HostConfig.Binds {
		if src, _, _ := strings.Cut(b, ":"); !strings.HasPrefix(src, "/") {
			names = append(names, src)
		}
	}
	for _, m := range create.HostConfig.Mounts {
		if m.Type == "volume" && m.Source != "" {
			names = append(names, m.Source)
		}
	}
	for _, name := range names {
		if d.find("volumes", name) == nil {
			d.mu.Lock()
			d.volumesMissing = append(d.volumesMissing, name)
			d.mu.Unlock()
		}
	}
}

// find looks an object up as dockerd does: by full ID, then by name, then
// by a unique ID prefix; a volume by its name only.
func (d *daemon) find(kind, ref string) *object {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, o := range d.objects {
		if o.kind == kind && o.id == ref {
			return o
		}
	}
	for _, o := range d.objects {
		if o.kind == kind && o.name == ref {
			return o
		}
	}
	if kind == "volumes" {
		return nil
	}
	var hit *object
	for _, o := range d.objects {
		if o.kind == kind && strings.HasPrefix(o.id, ref) {
			if hit != nil {
				return nil
			}
			hit = o
		}
	}
	return hit
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
	sock string // the gate's
}

// newGate serves a Gate for task testID in front of a fake daemon.
func newGate(t *testing.T) *gateClient {
	t.Helper()
	d, upstream := newDaemon(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The task's own container is the daemon's "agent" (atl names it
	// atl-<id>, see ContainerName).
	g := &Gate{ID: testID, Root: root, SocketHost: "/host/run/docker.sock", Agent: "agent", Upstream: upstream}
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
	return &gateClient{t: t, http: client, root: root, d: d, sock: sock}
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
		"another's by prefix":  `{"HostConfig":{"NetworkMode":"container:abcd"}}`,
		"another's endpoint ID": `{"HostConfig":{"NetworkMode":"bridge"},` +
			`"NetworkingConfig":{"EndpointsConfig":{"hostnet":{"NetworkID":"` + mynetID + `"}}}}`,
		"syslog log driver": `{"HostConfig":{"LogConfig":{"Type":"syslog","Config":{"syslog-address":"tcp://127.0.0.1:22"}}}}`,
		"gelf log driver":   `{"HostConfig":{"LogConfig":{"Type":"gelf","Config":{"gelf-address":"udp://127.0.0.1:1"}}}}`,
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
		"PortBindings":{"5432/tcp":[{"HostPort":"5732"}]},"RestartPolicy":{"Name":"no"},"MaskedPaths":null,
		"VolumesFrom":["mine:ro"],"LogConfig":{"Type":"json-file","Config":{"max-size":"1m"}}},
		"NetworkingConfig":{"EndpointsConfig":{"mynet":{"NetworkID":"hostnet","Aliases":["db"]}}}}`
	status, out := c.do("POST", "/v1.45/containers/create?name=db", body)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d: %s", status, out)
	}
	var got struct {
		Labels     map[string]string
		HostConfig struct {
			Binds       []string
			Memory      json.Number
			NetworkMode string
			VolumesFrom []string
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]struct {
				NetworkID string
				Aliases   []string
			}
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
	// Containers and networks go to the daemon by the IDs the checks found,
	// the endpoint's NetworkID (which the daemon prefers) included.
	if got.HostConfig.NetworkMode != mynetID || !slices.Equal(got.HostConfig.VolumesFrom, []string{mineID + ":ro"}) {
		t.Errorf("network mode %q, volumes from %q; want them by ID", got.HostConfig.NetworkMode, got.HostConfig.VolumesFrom)
	}
	if ep, ok := got.NetworkingConfig.EndpointsConfig[mynetID]; !ok || ep.NetworkID != mynetID || len(got.NetworkingConfig.EndpointsConfig) != 1 ||
		!slices.Equal(ep.Aliases, []string{"db"}) {
		t.Errorf("endpoints = %+v; want mynet's by its ID, with its NetworkID and aliases", got.NetworkingConfig.EndpointsConfig)
	}
	if status, out := c.do("POST", "/containers/create", `{"HostConfig":{"NetworkMode":"container:agent"}}`); status != http.StatusCreated {
		t.Fatalf("create in the own container's network: %d %s", status, out)
	}
	if b := c.d.body("POST /containers/create"); !strings.Contains(b, `"NetworkMode":"container:`+agentID+`"`) {
		t.Errorf("create body = %s; want the network mode by the container's ID", b)
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
		{"GET", "/containers/missing/json", 404}, // the daemon's own status
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
	if _, out := c.do("GET", "/networks/mynet", ""); strings.Contains(out, `"agent"`) || !strings.Contains(out, `"db"`) {
		t.Errorf("network inspect = %s", out)
	}
	// What the gate forwards names the objects by the full IDs its checks
	// found, never by the task's reference.
	for _, tc := range []struct{ method, uri, want string }{
		{"POST", "/containers/mine/stop", "POST /v1.45/containers/" + mineID + "/stop"},
		{"GET", "/containers/agent/json", "GET /v1.45/containers/" + agentID + "/json"},
		{"GET", "/networks/mynet", "GET /v1.45/networks/" + mynetID},
		{"POST", "/networks/mynet/connect", "POST /v1.45/networks/" + mynetID + "/connect"},
		{"GET", "/volumes/myvol", "GET /v1.45/volumes/myvol"},
		{"DELETE", "/containers/mine", "DELETE /v1.45/containers/" + mineID},
		{"DELETE", "/networks/mynet", "DELETE /v1.45/networks/" + mynetID},
	} {
		body := ""
		if strings.HasSuffix(tc.uri, "/connect") {
			body = `{"Container":"agent","EndpointConfig":{"NetworkID":"hostnet","Aliases":["a"]}}`
		}
		if status, out := c.do(tc.method, "/v1.45"+tc.uri, body); status >= 400 {
			t.Errorf("%s %s: status %d (%s)", tc.method, tc.uri, status, out)
		}
		if got := c.d.saw(tc.want); got == "" {
			t.Errorf("%s %s did not reach the daemon as %s", tc.method, tc.uri, tc.want)
		}
	}
	// The endpoint's NetworkID, which the daemon prefers to the network in
	// the path, is the checked network's; the container goes by its ID.
	if b := c.d.body("POST /v1.45/networks/" + mynetID + "/connect"); !strings.Contains(b, `"NetworkID":"`+mynetID+`"`) ||
		!strings.Contains(b, `"Container":"`+agentID+`"`) {
		t.Errorf("connect body = %s", b)
	}
}

// TestGateChecksWhatItForwards: a reference that names one of the task's
// objects when the gate checks it, and another (the box, say) when the
// daemon resolves it, cannot reach the other. The task holds back the body
// of an exec create while the gate checks the path, renames its container
// to a prefix of another container's ID, then sends the body; and the same
// with a network it deletes in between.
func TestGateChecksWhatItForwards(t *testing.T) {
	c := newGate(t)
	sock := c.sock
	// Control: by the box's ID prefix, refused.
	if status, _ := c.do("POST", "/v1.45/containers/abcd/exec", `{"Cmd":["id"]}`); status != http.StatusForbidden {
		t.Errorf("exec by the box's ID prefix: %d", status)
	}
	// The task's container is named "abcd", a prefix of the box's ID.
	if status, out := c.do("POST", "/v1.45/containers/mine/rename?name=abcd", ""); status >= 400 {
		t.Fatalf("rename: %d %s", status, out)
	}
	send := func(method, uri, body string, between func()) (int, string) {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			method, uri, len(body), body[:1])
		time.Sleep(200 * time.Millisecond) // the gate has the head
		between()
		io.WriteString(conn, body[1:])
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	status, out := send("POST", "/v1.45/containers/abcd/exec", `{"Cmd":["sh","-c","id"],"User":"0"}`, func() {
		if status, out := c.do("POST", "/v1.45/containers/abcd/rename?name=elsewhere", ""); status >= 400 {
			t.Fatalf("rename: %d %s", status, out)
		}
	})
	c.d.mu.Lock()
	execIn := slices.Clone(c.d.execIn)
	c.d.mu.Unlock()
	if slices.Contains(execIn, hostID) {
		t.Errorf("an exec was created in the box container (answer %d %s)", status, out)
	}
	if status == http.StatusCreated && !slices.Equal(execIn, []string{mineID}) {
		t.Errorf("exec created in %v, want only in the task's own (%s)", execIn, mineID)
	}

	// The network variant: the task's network "abcd" (a prefix of another
	// task's network ID), connected to and deleted in between.
	c.d.mu.Lock()
	c.d.objects = append(c.d.objects, &object{"networks", "9999999999999999999999999999999999999999999999999999999999999999", "abcd",
		map[string]string{LabelTask: testID}})
	c.d.mu.Unlock()
	status, out = send("POST", "/v1.45/networks/abcd/connect", `{"Container":"agent"}`, func() {
		if status, out := c.do("DELETE", "/v1.45/networks/abcd", ""); status >= 400 {
			t.Fatalf("delete: %d %s", status, out)
		}
	})
	if got := c.d.saw("POST /v1.45/networks/" + hostnetID); got != "" {
		t.Errorf("the connect reached another's network: %s", got)
	}
	if got := c.d.saw("POST /v1.45/networks/abcd/"); got != "" {
		t.Errorf("the connect was forwarded by name: %s (answer %d %s)", got, status, out)
	}
}

// TestGateVolumeDeleteWaitsForCreate: a container create that names a volume
// of the task's reaches the daemon while that volume still exists. The task
// deletes the volume through the gate right after the gate checked it for
// the create; were the delete let through first, the daemon would create a
// fresh volume of that name for the container, without the task's label,
// and atl done would never remove it. The delete waits for the create
// instead, and still happens.
func TestGateVolumeDeleteWaitsForCreate(t *testing.T) {
	c := newGate(t)
	deleted := make(chan int, 1)
	deleteVolume := func() {
		req, _ := http.NewRequest("DELETE", "http://gate/v1.45/volumes/myvol", nil)
		resp, err := c.http.Do(req)
		if err != nil {
			deleted <- 0
			return
		}
		resp.Body.Close()
		deleted <- resp.StatusCode
	}
	var first atomic.Bool
	c.d.mu.Lock()
	c.d.onInspect = func(kind, ref string) {
		if kind != "volumes" || ref != "myvol" || first.Swap(true) {
			return
		}
		// The create's check: the task deletes the volume now.
		go deleteVolume()
		time.Sleep(500 * time.Millisecond) // time enough for an unhindered delete
	}
	c.d.mu.Unlock()
	status, out := c.do("POST", "/v1.45/containers/create", `{"Image":"x","HostConfig":{"Binds":["myvol:/data"]}}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, out)
	}
	select {
	case s := <-deleted:
		if s != http.StatusNoContent {
			t.Errorf("the delete after the create: status %d", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the volume delete never finished")
	}
	c.d.mu.Lock()
	missing := slices.Clone(c.d.volumesMissing)
	c.d.mu.Unlock()
	if len(missing) > 0 {
		t.Errorf("the create reached the daemon after volume %v was deleted: the daemon makes it again, unlabelled", missing)
	}
}

// TestGateReservesTaskContainerNames: a task can neither create a container
// under the name atl gives a task's own container (atl-<slug>-<hash>) nor
// rename one to it, however Docker would read the name: with its leading
// slash, in another case, given twice, or in a form body, which Docker reads
// before the query.
func TestGateReservesTaskContainerNames(t *testing.T) {
	c := newGate(t)
	victim := "atl-victim-1a2b3c"
	for _, name := range []string{victim, "/" + victim, "//" + victim, "ATL-Victim-1A2B3C", "atl-" + testID} {
		q := "?name=" + url.QueryEscape(name)
		if status, out := c.do("POST", "/v1.45/containers/create"+q, `{"Image":"x"}`); status != http.StatusForbidden {
			t.Errorf("create named %q: status %d (%s), want a refusal", name, status, out)
		}
		if status, out := c.do("POST", "/v1.45/containers/mine/rename"+q, ""); status != http.StatusForbidden {
			t.Errorf("rename to %q: status %d (%s), want a refusal", name, status, out)
		}
	}
	twice := "?name=db&name=" + victim
	if status, _ := c.do("POST", "/v1.45/containers/create"+twice, `{"Image":"x"}`); status != http.StatusForbidden {
		t.Errorf("create with the name twice: status %d", status)
	}
	if status, _ := c.do("POST", "/v1.45/containers/mine/rename"+twice, ""); status != http.StatusForbidden {
		t.Errorf("rename with the name twice: status %d", status)
	}
	// Docker reads a form body into the request's values before the query.
	for _, ct := range []string{"application/x-www-form-urlencoded", "Application/X-WWW-Form-Urlencoded; charset=utf-8", "multipart/form-data; boundary=x"} {
		req, _ := http.NewRequest("POST", "http://gate/v1.45/containers/mine/rename?name=fine", strings.NewReader("name="+victim))
		req.Header.Set("Content-Type", ct)
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("rename with a %s body: status %d, want a refusal", ct, resp.StatusCode)
		}
	}
	for _, r := range c.d.requests {
		if strings.HasPrefix(r, "POST /v1.45/containers/create") || strings.Contains(r, "/rename") {
			t.Errorf("reached the daemon: %s", r)
		}
	}
	// Other names are the task's to use.
	for _, name := range []string{"db", "atl-db", "atlas-1a2b3c", "stack-atl-x-1a2b3c-1"} {
		if status, out := c.do("POST", "/v1.45/containers/create?name="+name, `{"Image":"x"}`); status != http.StatusCreated {
			t.Errorf("create named %q: status %d (%s)", name, status, out)
		}
	}
	if status, out := c.do("POST", "/v1.45/containers/mine/rename?name=db2", ""); status >= 400 {
		t.Errorf("rename to db2: status %d (%s)", status, out)
	}
}

// TestGateOwnContainerByLabelsOnly: a container is the task's own one only
// if it has atelier.agent=<id> and no atelier.task label. A task cannot set
// atelier.agent on a container through its gate, but a container takes the
// labels of its image, which the task can build.
func TestGateOwnContainerByLabelsOnly(t *testing.T) {
	c := newGate(t)
	c.d.mu.Lock()
	c.d.objects = append(c.d.objects, &object{"containers", "5555555555555555555555555555555555555555555555555555555555555555", "fake-own",
		map[string]string{LabelAgent: testID, LabelTask: "someone-else"}})
	c.d.mu.Unlock()
	for _, tc := range []struct{ method, uri string }{
		{"GET", "/containers/fake-own/json"},
		{"POST", "/containers/fake-own/exec"},
	} {
		if status, out := c.do(tc.method, "/v1.45"+tc.uri, "{}"); status != http.StatusForbidden {
			t.Errorf("%s %s: status %d (%s), want 403", tc.method, tc.uri, status, out)
		}
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
		{"POST", "/build?remote=" + url.QueryEscape("git@github.com:org/private.git"), ""},
		{"POST", "/build?remote=" + url.QueryEscape("https://example.com/context.tar"), ""},
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
	if b := c.d.body("POST /networks/new1/connect"); !strings.Contains(b, `"`+agentID+`"`) {
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
