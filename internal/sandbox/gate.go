package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Labels the gate puts on Docker objects. Their value is the sandbox ID.
const (
	LabelTask  = "atelier.task"  // containers, networks and volumes a task created
	LabelAgent = "atelier.agent" // the task's own container, created by atl
)

// Gate is a task's Docker socket. It forwards to the host daemon only what a
// task may do: everything the task creates is labelled, the task sees and
// touches only what carries its label, mounts must come from the task's own
// directory (or, read-only, from the paths of $ATL_SANDBOX_MOUNTS), and every
// container setting that reaches into the host is refused. A call that is
// not in gateRoutes is refused.
//
// Known gap: a task can still swap a mount source for a symlink between the
// gate's check and the daemon's mount. Closing it needs the task directory on
// a volume, with mounts rewritten to volume subpaths the daemon resolves.
// BuildKit sessions are not gated either, so builds need DOCKER_BUILDKIT=0.
type Gate struct {
	ID         string      // sandbox ID, the value of the labels
	Root       string      // task directory: the only place mounts may come from
	SocketHost string      // the gate's socket as the host daemon sees it; a mount of the Docker socket becomes a mount of this
	Agent      string      // name of the task's own container: joined (by the ID its labels are checked on) to every network the task creates
	Upstream   string      // host daemon socket
	Log        *log.Logger // refusals (and every call if Verbose); nil: discarded
	Verbose    bool
	Ident      string // which task and atl binary this gate serves (see Ident); Serve writes it to the run dir
	// ROMounts are host paths (symlinks resolved, see ROMounts) a mount may
	// also come from, or from inside, read-only only: a read-write request
	// is made read-only.
	ROMounts []string
	// NoDocker refuses every Docker request: the assistant's gate, whose
	// container has no Docker access at all (Root is then the assistant
	// dir, which the gate stops without; Upstream is not used).
	NoDocker bool
	// Reserved returns the image names atl runs its own containers from,
	// besides DefaultImage and BoxImage: the workspace's task image and
	// every project's. A task may not create or move these tags (see
	// checkTag); an error refuses every request that would.
	Reserved func() ([]string, error)
	// Registries returns the registries a task may pull from (docker
	// pull, a build's FROM and COPY --from), besides DefaultRegistries
	// (see checkRegistry); an error refuses every pull.
	Registries func() ([]string, error)
	// HostRoutes returns the routes of the host the daemon runs on, which a
	// network's subnet may not overlap (see checkIPAM); nil: hostRoutes,
	// read where the gate runs.
	HostRoutes func() ([]netip.Prefix, error)

	once   sync.Once
	client *http.Client
	proxy  *httputil.ReverseProxy

	execMu  sync.Mutex
	execs   map[string]bool // exec IDs created through this gate (see addExec)
	execsAt int             // size of execs at which addExec prunes it next
	pruning bool            // a pruneExecs runs

	// volumes orders the task's requests that name volumes. A volume is
	// the one object the gate forwards by name (its name is its ID), and
	// the name stays free for anyone once it is deleted: the daemon
	// creates a volume a container create names if none exists, without
	// the task's label, and another task may create one of that name. So
	// a request that checks a volume holds the read lock from its check
	// until the daemon has answered it (a container create, a volume
	// inspect), and a volume delete holds the write lock for its own
	// check and forward: no delete runs in between. Containers and
	// networks are forwarded by full IDs, which are never reused, so a
	// delete between their check and their forward only makes the
	// forward fail, and the requests that stream (attach, exec start,
	// logs, events, builds, pulls) hold nothing.
	volumes sync.RWMutex
}

type gateHandler func(g *Gate, w http.ResponseWriter, r *http.Request, m []string)

type gateRoute struct {
	methods string
	re      func() *regexp.Regexp // compiled on first use: not in every atl process (hooks)
	h       gateHandler
}

func route(methods, pattern string, h gateHandler) gateRoute {
	return gateRoute{methods, sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("^" + pattern + "$") }), h}
}

// gateRoutes is everything a task may call, matched without the /v1.xx prefix.
var gateRoutes = []gateRoute{
	route("GET HEAD", `/_ping`, gatePass),
	route("GET", `/(version|info)`, gatePass),
	route("GET", `/events`, gateList),

	route("GET", `/containers/json`, gateList),
	route("POST", `/containers/create`, gateContainerCreate),
	route("GET", `/containers/([^/]+)/(json|logs|top|stats|changes)`, gateContainer),
	route("POST", `/containers/([^/]+)/(start|stop|kill|restart|wait|attach|resize|rename|pause|unpause)`, gateContainer),
	route("GET HEAD PUT", `/containers/([^/]+)/(archive)`, gateContainer),
	route("DELETE", `/containers/([^/]+)`, gateContainer),
	route("POST", `/containers/([^/]+)/exec`, gateExecCreate),
	route("POST", `/exec/([^/]+)/(start|resize)`, gateExec),
	route("GET", `/exec/([^/]+)/json`, gateExec),

	// Images are shared between tasks: reading is fine, removing is not; a
	// pull or a build may only name a tag that is neither atl's nor
	// another's (checkTag). Tagging, importing, loading and committing are
	// not in this table: refused.
	route("GET", `/images/json`, gatePass),
	route("GET", `/images/(.+)/(json|history)`, gatePass),
	route("GET", `/distribution/(.+)/json`, gateDistribution),
	route("POST", `/images/create`, gateImagePull),
	route("POST", `/build`, gateBuild),

	route("GET", `/networks`, gateList),
	route("POST", `/networks/create`, gateNetworkCreate),
	route("GET", `/networks/([^/]+)`, gateNetwork),
	route("DELETE", `/networks/([^/]+)`, gateNetworkDelete),
	route("POST", `/networks/([^/]+)/(connect|disconnect)`, gateNetworkConnect),

	route("GET", `/volumes`, gateList),
	route("POST", `/volumes/create`, gateVolumeCreate),
	route("GET DELETE", `/volumes/([^/]+)`, gateVolume),
}

// agentActions is what a task may do with its own container: look at it, run
// commands in it and copy files. Stopping or removing it would end the task's
// sessions, e.g. from a clean-up of "all my containers".
var agentActions = map[string]bool{"json": true, "logs": true, "top": true, "stats": true, "changes": true, "resize": true, "archive": true}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func (g *Gate) init() {
	g.once.Do(func() {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", g.Upstream)
		}}
		g.client = &http.Client{Transport: transport}
		g.proxy = &httputil.ReverseProxy{
			Director:      func(r *http.Request) { r.URL.Scheme, r.URL.Host, r.Host = "http", "docker", "docker" },
			Transport:     transport,
			FlushInterval: -1, // logs, events and pull progress are streams
		}
	})
}

func (g *Gate) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log.Printf(format, args...)
	}
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.init()
	if g.NoDocker {
		g.deny(w, r, http.StatusForbidden, "the assistant has no Docker access")
		return
	}
	if strings.Contains(r.URL.EscapedPath(), "%") {
		g.deny(w, r, http.StatusForbidden, "encoded characters in the path are not accepted")
		return
	}
	if formBody(r) {
		// dockerd reads a form body into the request's values ahead of the
		// query, which is what the gate checks (a container's new name, a
		// build's remote, ...). No Docker client sends one.
		g.deny(w, r, http.StatusForbidden, "form bodies are not accepted; parameters go in the query")
		return
	}
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	for _, rt := range gateRoutes {
		for _, method := range strings.Fields(rt.methods) {
			if method != r.Method {
				continue
			}
			if m := rt.re().FindStringSubmatch(path); m != nil {
				rt.h(g, w, r, m)
				return
			}
		}
	}
	g.deny(w, r, http.StatusForbidden, "this Docker API call is not available to tasks")
}

// formBody reports whether a Content-Type of r makes Go's net/http, and so
// dockerd, parse the body as form values: its media type (before any
// parameters, trimmed, in any case, as mime.ParseMediaType reads it, which
// dockerd also follows when the parameters are invalid) is a form.
func formBody(r *http.Request) bool {
	for _, ct := range r.Header.Values("Content-Type") {
		mt, _, _ := strings.Cut(ct, ";")
		if oneOf(strings.ToLower(strings.TrimSpace(mt)), "application/x-www-form-urlencoded", "multipart/form-data") {
			return true
		}
	}
	return false
}

// checkName refuses a container name in the query (all of them, should it
// be given twice) that is in the pattern of atl's own task containers
// (ReservedName): another task's session would run in a container the task
// made, and get its Claude token there. A name in the pattern of a task's
// namespace (Namespaced) must be in this task's: compose names a project's
// containers <project>-<service>-<n>, and another task's compose would
// find its names taken. Other names are free.
func (g *Gate) checkName(r *http.Request) error {
	for _, name := range r.URL.Query()["name"] {
		if ReservedName(name) {
			return fmt.Errorf("container names like %s are reserved for atl's task containers", name)
		}
		// dockerd strips one leading slash (and refuses a second).
		if ns := g.namespace(); Namespaced(name) && !InNamespace(ns, strings.TrimPrefix(name, "/")) {
			return fmt.Errorf("container name %s is in another task's compose namespace: a name that starts with atl_<6 hex digits> "+
				"must start with %s (this task's compose project; COMPOSE_PROJECT_NAME is set)", name, ns)
		}
	}
	return nil
}

// namespace is the task's Docker name namespace (see ComposeProject).
func (g *Gate) namespace() string { return ComposeProject(g.ID) }

// checkNewName refuses a name for a new network or volume (what) that is
// not in the task's namespace (see InNamespace): tasks' names would collide,
// and a compose project on the host would adopt a network or volume of the
// name it looks for, made (and filled, or joined) by the task.
func (g *Gate) checkNewName(what, name string) error {
	ns := g.namespace()
	if InNamespace(ns, name) {
		return nil
	}
	instead := ns + "_<name>"
	if name != "" {
		instead = ns + "_" + name
	}
	return fmt.Errorf("%s name %q: new network and volume names must be %s or start with %s_ or %s- "+
		"(this task's compose project; COMPOSE_PROJECT_NAME is set): name it %s, or use -p %s-<name> for another compose project",
		what, name, ns, ns, ns, instead, ns)
}

// randomName matches a canonical lower-case UUID, which a network may also
// be named, outside the namespace: testcontainers names its networks so
// (Java's Network.newNetwork(), testcontainers-go's network.New, Node,
// Python). No task can guess another's to take it first, and compose names
// none so (<project>_<network>).
var randomName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
})

// composeProject is the label by which compose finds the containers,
// networks and volumes of a project.
const composeProject = "com.docker.compose.project"

// checkProjectLabel refuses labels (a create's Labels) whose compose
// project is not in the task's namespace: a compose project on the host
// would take the object for its own. The key is read exactly, as dockerd
// stores labels and compose filters them (another spelling is another
// label, which compose does not read; the gate forwards the map it read,
// so a key given twice is the one it checked). An empty value is no
// project.
func (g *Gate) checkProjectLabel(labels any) error {
	m, err := as[map[string]any]("Labels", labels)
	if err != nil {
		return err
	}
	v, ok := m[composeProject]
	if !ok {
		return nil
	}
	project, isString := v.(string)
	if !isString {
		return fmt.Errorf("label %s has the wrong type", composeProject)
	}
	if ns := g.namespace(); project != "" && !InNamespace(ns, project) {
		return fmt.Errorf("label %s=%s: the compose project must be %s or start with %s_ or %s- "+
			"(this task's; COMPOSE_PROJECT_NAME is set): use -p %s-<name> for another compose project",
			composeProject, project, ns, ns, ns, ns)
	}
	return nil
}

func (g *Gate) deny(w http.ResponseWriter, r *http.Request, status int, reason string) {
	g.logf("deny  %s %s: %s", r.Method, r.URL.Path, reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": "atelier gate: " + reason})
}

func (g *Gate) allowed(r *http.Request) {
	if g.Verbose {
		g.logf("allow %s %s", r.Method, r.URL.Path)
	}
}

// forward sends the request on, with body (if not nil) in place of the original.
func (g *Gate) forward(w http.ResponseWriter, r *http.Request, body any) {
	if body != nil {
		b, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.TransferEncoding = nil
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
		r.Header.Set("Content-Type", "application/json")
	}
	g.allowed(r)
	g.proxy.ServeHTTP(w, r)
}

// call talks to the host daemon on the gate's own behalf.
func (g *Gate) call(method, uri string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://docker"+uri, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// relay is forward for the calls whose answer the gate needs to read.
func (g *Gate) relay(w http.ResponseWriter, r *http.Request, body any) (int, []byte) {
	status, out, err := g.call(r.Method, r.URL.RequestURI(), body)
	if err != nil {
		g.deny(w, r, http.StatusBadGateway, err.Error())
		return 0, nil
	}
	g.reply(w, r, status, out)
	return status, out
}

func (g *Gate) reply(w http.ResponseWriter, r *http.Request, status int, out []byte) {
	g.allowed(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(out)
}

// stream relays the calls that turn the connection into a raw byte stream
// (attach, exec start). The standard reverse proxy drops such a stream as
// soon as one side stops sending, which loses the output of a container
// without stdin, so the gate copies it by hand. A reply that is not a raw
// stream ends the connection: nothing may follow on it unchecked.
func (g *Gate) stream(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		g.deny(w, r, http.StatusInternalServerError, "connection cannot be taken over")
		return
	}
	back, err := net.Dial("unix", g.Upstream)
	if err != nil {
		g.deny(w, r, http.StatusBadGateway, err.Error())
		return
	}
	defer back.Close()
	r.Host = "docker"
	if err := r.Write(back); err != nil {
		g.deny(w, r, http.StatusBadGateway, err.Error())
		return
	}
	fromBack := bufio.NewReader(back)
	resp, err := http.ReadResponse(fromBack, r)
	if err != nil {
		g.deny(w, r, http.StatusBadGateway, err.Error())
		return
	}
	g.allowed(r)
	conn, client, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	raw := resp.StatusCode == http.StatusSwitchingProtocols ||
		strings.HasPrefix(resp.Header.Get("Content-Type"), "application/vnd.docker.")
	if !raw {
		resp.Close = true
		resp.Write(conn)
		return
	}
	fmt.Fprintf(conn, "HTTP/1.1 %s\r\n", resp.Status)
	resp.Header.Write(conn)
	io.WriteString(conn, "\r\n")
	done := make(chan struct{})
	defer close(done)
	go func() { // client to daemon; when the client is done sending, say so and keep reading
		io.Copy(back, client)
		if c, ok := back.(*net.UnixConn); ok {
			c.CloseWrite()
		}
		// ...until it hangs up: a quiet container would otherwise hold
		// the stream until it next prints or exits.
		if awaitHangup(conn, done) {
			back.Close()
		}
	}()
	io.Copy(conn, fromBack)
}

// hangupCheck is how often awaitHangup looks.
const hangupCheck = 500 * time.Millisecond

// awaitHangup waits until conn, a client that has stopped sending, hangs
// up altogether, and reports whether it did; false once done is closed. A
// unix socket tells that apart from a client that only shut its sending
// side and still reads (docker run without -i): POLLHUP. Other conns are
// not watched.
func awaitHangup(conn net.Conn, done <-chan struct{}) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	tick := time.NewTicker(hangupCheck)
	defer tick.Stop()
	for {
		hup := false
		err := rc.Control(func(fd uintptr) {
			fds := []unix.PollFd{{Fd: int32(fd)}}
			if n, _ := unix.Poll(fds, 0); n > 0 {
				hup = fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0
			}
		})
		switch {
		case err != nil:
			return false
		case hup:
			return true
		}
		select {
		case <-done:
			return false
		case <-tick.C:
		}
	}
}

func decodeBody(r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber() // keep 64-bit numbers intact on the way through
	if err := dec.Decode(&body); err != nil && err != io.EOF {
		return nil, fmt.Errorf("unreadable request body: %v", err)
	}
	if body == nil { // "null"
		body = map[string]any{}
	}
	return body, nil
}

// fieldKeys refuses an object of a request body that has a key dockerd
// would take for one of fields, the names the gate reads it by, without
// being spelled exactly so. dockerd decodes request bodies with
// encoding/json, which matches a key to a field in any case, Unicode case
// folding included ("hoſtconfig" is HostConfig), and lets a later spelling
// add to or override what an earlier one set; the gate, reading a map,
// sees only the exact name. Docker's clients send the exact names.
func fieldKeys(obj map[string]any, fields ...string) error {
	for k := range obj {
		for _, f := range fields {
			if k != f && strings.EqualFold(k, f) {
				return fmt.Errorf("setting %q is not accepted: spell it %s", k, f)
			}
		}
	}
	return nil
}

var errNotFound = errors.New("not found")

// owner returns who a container, network or volume belongs to: "task" if it
// carries this task's label, "agent" if it is the task's own container
// (atelier.agent=<ID> and no atelier.task label, see Agent). Any other
// object is an error; errNotFound if it does not exist.
//
// It also returns the object's full ID (a volume's name), from the same
// inspect that showed the label. What the gate forwards must name the
// object by that ID, never by ref: the daemon resolves a reference by full
// ID, then by name, then by unique ID prefix, so the object a name or a
// prefix stands for can change between this check and the call (a task
// renames its own container to a prefix of another container's ID, or
// removes its network whose name is a prefix of another's). A volume is
// looked up by its exact name only: its name is its ID.
func (g *Gate) owner(kind, ref string) (who, id string, err error) {
	uri := "/" + kind + "/" + url.PathEscape(ref)
	if kind == "containers" {
		uri += "/json"
	}
	status, out, err := g.call("GET", uri, nil)
	switch {
	case err != nil:
		return "", "", err
	case status == http.StatusNotFound:
		return "", "", errNotFound
	case status != http.StatusOK:
		return "", "", fmt.Errorf("cannot inspect %s", ref)
	}
	var obj struct {
		Id, Name string
		Labels   map[string]string
		Config   struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return "", "", err
	}
	labels, id := obj.Labels, obj.Id
	switch kind {
	case "containers":
		labels = obj.Config.Labels
	case "volumes":
		id = obj.Name
	}
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return "", "", fmt.Errorf("cannot inspect %s", ref)
	}
	_, isTasks := labels[LabelTask]
	switch {
	case labels[LabelTask] == g.ID:
		return "task", id, nil
	case kind == "containers" && labels[LabelAgent] == g.ID && !isTasks:
		// atl's, as Agent checks it: a container a task created carries
		// atelier.task, and may carry atelier.agent from its image.
		return "agent", id, nil
	}
	return "", "", fmt.Errorf("%s %q does not belong to this task", strings.TrimSuffix(kind, "s"), ref)
}

// owned returns the full ID of an object that carries this task's label.
func (g *Gate) owned(kind, ref string) (string, error) {
	who, id, err := g.owner(kind, ref)
	if err == nil && who != "task" {
		return "", fmt.Errorf("%q is the task's own container", ref)
	}
	return id, err
}

// checkOwned returns the full ID of an object of the task's, or answers the
// request itself: forbidden for another's, the daemon's own "no such"
// status for one that does not exist (the gate never forwards a reference
// it could not pin to an ID).
func (g *Gate) checkOwned(w http.ResponseWriter, r *http.Request, kind, ref string) (string, bool) {
	id, err := g.owned(kind, ref)
	if err != nil {
		g.refuseRef(w, r, kind, ref, err)
		return "", false
	}
	return id, true
}

// refuseRef answers a request whose object reference did not check out.
func (g *Gate) refuseRef(w http.ResponseWriter, r *http.Request, kind, ref string, err error) {
	if err == errNotFound {
		g.deny(w, r, http.StatusNotFound, "No such "+strings.TrimSuffix(kind, "s")+": "+ref)
		return
	}
	g.deny(w, r, http.StatusForbidden, err.Error())
}

// setPath replaces the request's path (after the API version prefix, which
// is kept) with p, built from checked full IDs.
func setPath(r *http.Request, p string) {
	r.URL.Path = versionPrefix.FindString(r.URL.Path) + p
	r.URL.RawPath = ""
}

func gatePass(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) { g.forward(w, r, nil) }

// gateList narrows a listing (or the event stream) to what carries the task's label.
func gateList(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	q := r.URL.Query()
	filters := map[string]any{}
	if s := q.Get("filters"); s != "" {
		if err := json.Unmarshal([]byte(s), &filters); err != nil {
			g.deny(w, r, http.StatusBadRequest, "unreadable filters")
			return
		}
		if filters == nil { // "null"
			filters = map[string]any{}
		}
	}
	own := LabelTask + "=" + g.ID
	switch v := filters["label"].(type) {
	case map[string]any: // {"label": {"a=b": true}}
		v[own] = true
	case []any: // {"label": ["a=b"]}
		filters["label"] = append(v, own)
	default: // the daemon refuses a mix of the two forms, so follow the other filters
		filters["label"] = map[string]bool{own: true}
		for _, other := range filters {
			if _, isList := other.([]any); isList {
				filters["label"] = []string{own}
			}
		}
	}
	b, _ := json.Marshal(filters)
	q.Set("filters", string(b))
	r.URL.RawQuery = q.Encode()
	g.forward(w, r, nil)
}

func gateContainer(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	action := ""
	if len(m) > 2 {
		action = m[2]
	}
	who, id, err := g.owner("containers", m[1])
	switch {
	case err != nil:
		g.refuseRef(w, r, "containers", m[1], err)
		return
	case who == "agent" && !agentActions[action]:
		g.deny(w, r, http.StatusForbidden, "this is the task's own container: it cannot be stopped, removed or renamed from inside")
		return
	}
	if action == "start" && (r.ContentLength > 7 || r.ContentLength == -1) {
		// What dockerd reads as a HostConfig: Docker 24 and older apply
		// it for API versions before 1.24; later ones refuse it.
		g.deny(w, r, http.StatusForbidden, "a container start takes no settings; they go in the create")
		return
	}
	if action == "start" && slices.ContainsFunc(r.URL.Query()["checkpoint-dir"], func(s string) bool { return s != "" }) {
		// Docker 17.09 and older (experimental) restore the container
		// from a checkpoint in this host directory; later ones refuse it.
		g.deny(w, r, http.StatusForbidden, "a checkpoint directory is not allowed")
		return
	}
	if action == "rename" {
		if err := g.checkName(r); err != nil {
			g.deny(w, r, http.StatusForbidden, err.Error())
			return
		}
	}
	if action == "" {
		setPath(r, "/containers/"+id)
	} else {
		setPath(r, "/containers/"+id+"/"+action)
	}
	if action == "attach" {
		g.stream(w, r)
		return
	}
	g.forward(w, r, nil)
}

// gateNetwork shows a network of the task without the task's own container
// in it: the gate attached that one, and compose leaves a network alone when
// it sees a container it does not know.
func gateNetwork(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	id, ok := g.checkOwned(w, r, "networks", m[1])
	if !ok {
		return
	}
	setPath(r, "/networks/"+id)
	status, out, err := g.call("GET", r.URL.RequestURI(), nil)
	if err != nil {
		g.deny(w, r, http.StatusBadGateway, err.Error())
		return
	}
	var network map[string]any
	if status == http.StatusOK && g.Agent != "" && json.Unmarshal(out, &network) == nil {
		containers, _ := network["Containers"].(map[string]any)
		for id, c := range containers {
			if info, _ := c.(map[string]any); info["Name"] == g.Agent {
				delete(containers, id)
			}
		}
		out, _ = json.Marshal(network)
	}
	g.reply(w, r, status, out)
}

// gateVolume inspects or deletes a volume of the task's: a delete holds
// the volumes lock exclusively (see Gate.volumes).
func gateVolume(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if r.Method == "DELETE" {
		g.volumes.Lock()
		defer g.volumes.Unlock()
	} else {
		g.volumes.RLock()
		defer g.volumes.RUnlock()
	}
	if name, ok := g.checkOwned(w, r, "volumes", m[1]); ok {
		setPath(r, "/volumes/"+name)
		g.forward(w, r, nil)
	}
}

func gateExec(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if !g.ownExec(m[1]) {
		g.deny(w, r, http.StatusForbidden, "this exec was not started by this task")
		return
	}
	if len(m) > 2 && m[2] == "start" {
		g.stream(w, r)
		return
	}
	g.forward(w, r, nil)
}

// gateExecCreate allows commands in the task's containers and in its own
// container (as any user: root in there is how a task installs packages).
// The body is read before the container is looked up, and the exec is
// created in the container by the ID that lookup found (see owner).
func gateExecCreate(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := fieldKeys(body, "Privileged"); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	if !isZero(body["Privileged"]) {
		g.deny(w, r, http.StatusForbidden, "privileged exec is not allowed")
		return
	}
	_, id, err := g.owner("containers", m[1])
	if err != nil {
		g.refuseRef(w, r, "containers", m[1], err)
		return
	}
	setPath(r, "/containers/"+id+"/exec")
	if status, out := g.relay(w, r, body); status == http.StatusCreated {
		var created struct{ Id string }
		if json.Unmarshal(out, &created) == nil && created.Id != "" {
			g.addExec(created.Id)
		}
	}
}

// execsPruneMin is the least number of exec IDs the gate holds before it
// looks for the ones the daemon has forgotten.
const execsPruneMin = 256

// ownExec reports whether exec id was created through this gate.
func (g *Gate) ownExec(id string) bool {
	g.execMu.Lock()
	defer g.execMu.Unlock()
	return g.execs[id]
}

// addExec records an exec created through this gate. The daemon forgets an
// exec some minutes after it ended, or with its container; so that a long
// task's execs do not pile up here, each time execs has doubled since it
// was last pruned (and holds execsPruneMin), pruneExecs drops in the
// background those the daemon no longer knows.
func (g *Gate) addExec(id string) {
	g.execMu.Lock()
	defer g.execMu.Unlock()
	if g.execs == nil {
		g.execs = map[string]bool{}
	}
	g.execs[id] = true
	if g.pruning || len(g.execs) < max(g.execsAt, execsPruneMin) {
		return
	}
	g.pruning = true
	go g.pruneExecs(slices.Collect(maps.Keys(g.execs)))
}

// pruneExecs drops those of ids whose inspect the daemon answers with a
// 404: an exec it has forgotten, or one whose container is gone. Neither
// comes back (IDs are not reused), so the gate refuses only what the
// daemon would refuse anyway.
func (g *Gate) pruneExecs(ids []string) {
	var gone []string
	for _, id := range ids {
		if status, _, err := g.call("GET", "/exec/"+url.PathEscape(id)+"/json", nil); err == nil && status == http.StatusNotFound {
			gone = append(gone, id)
		}
	}
	g.execMu.Lock()
	defer g.execMu.Unlock()
	for _, id := range gone {
		delete(g.execs, id)
	}
	g.execsAt = 2 * len(g.execs)
	g.pruning = false
}

func gateBuild(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	q := r.URL.Query()
	switch {
	case q.Get("version") == "2":
		g.deny(w, r, http.StatusForbidden, "BuildKit sessions are not gated yet, use DOCKER_BUILDKIT=0")
	case !oneOf(q.Get("networkmode"), "", "default", "bridge", "none"):
		g.deny(w, r, http.StatusForbidden, "build network mode "+q.Get("networkmode")+" is not allowed")
	case q.Get("cgroupparent") != "":
		g.deny(w, r, http.StatusForbidden, "build cgroup parent is not allowed")
	case q.Get("remote") != "":
		// The daemon would fetch it itself, with the credentials of the
		// host it runs on.
		g.deny(w, r, http.StatusForbidden, "building from a remote URL is not allowed; build from a local context")
	case anyTrue(q["pull"]):
		// The builder would pull each FROM image and store it under its
		// name, past the image name rules of a pull (checkTag).
		g.deny(w, r, http.StatusForbidden, "build --pull is not allowed: pull the base image first with docker pull, then build")
	case anyNonEmpty(q["platform"]):
		// A FROM image of another platform than the local one is pulled
		// and stored under its name, as with --pull.
		g.deny(w, r, http.StatusForbidden, "build --platform is not allowed: pull the base image for that platform first, then build without it")
	default:
		if err := g.buildTags(r); err != nil {
			g.deny(w, r, http.StatusForbidden, err.Error())
			return
		}
		g.forwardBuild(w, r)
	}
}

// forwardBuild forwards a build once its context, which it reads into a
// file of its own, passes checkBuildContext and its base images pass
// pinBaseImages: the daemon gets those very bytes, or the context with the
// Dockerfile naming its base images by ID.
func (g *Gate) forwardBuild(w http.ResponseWriter, r *http.Request) {
	spool, err := os.CreateTemp("", "atl-build-context-")
	if err != nil {
		g.deny(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	os.Remove(spool.Name()) // the open file is all that is needed
	defer spool.Close()
	df, err := checkBuildContext(r, spool)
	if err == nil {
		var pinned *os.File
		if pinned, err = g.pinBaseImages(r, spool, df); pinned != nil {
			defer pinned.Close()
			spool = pinned
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		var refusal *buildRefusal
		if errors.As(err, &refusal) {
			status = http.StatusForbidden
		}
		g.deny(w, r, status, err.Error())
		return
	}
	size, err := spool.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = spool.Seek(0, io.SeekStart)
	}
	if err != nil {
		g.deny(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	r.Body = io.NopCloser(spool)
	r.ContentLength = size
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	g.forward(w, r, nil)
}

func (g *Gate) label(body map[string]any) {
	labels, _ := body["Labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[LabelTask] = g.ID
	delete(labels, LabelAgent) // a task cannot make a container its "own"
	body["Labels"] = labels
}

func gateVolumeCreate(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := fieldKeys(body, "Name", "Driver", "DriverOpts", "ClusterVolumeSpec", "Labels"); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	// The local driver's options can bind any host path (type=none,o=bind,device=/).
	if d, err := as[string]("Driver", body["Driver"]); err != nil || !oneOf(d, "", "local") || !isZero(body["DriverOpts"]) || !isZero(body["ClusterVolumeSpec"]) {
		g.deny(w, r, http.StatusForbidden, "volume drivers and driver options are not allowed")
		return
	}
	name, err := as[string]("Name", body["Name"])
	if err == nil && name != "" { // no name: the daemon makes one up (an anonymous volume)
		err = g.checkNewName("volume", name)
	}
	if err == nil {
		err = g.checkProjectLabel(body["Labels"])
	}
	if err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	g.label(body)
	g.forward(w, r, body)
}

var networkKeys = map[string]bool{
	"Name": true, "Driver": true, "Labels": true, "Internal": true, "Attachable": true,
	"EnableIPv4": true, "EnableIPv6": true, "IPAM": true, "CheckDuplicate": true, "Scope": true,
}

func gateNetworkCreate(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := fieldKeys(body, slices.Collect(maps.Keys(networkKeys))...); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	for k, v := range body {
		if !networkKeys[k] && !isZero(v) {
			g.deny(w, r, http.StatusForbidden, "network setting "+k+" is not allowed")
			return
		}
	}
	driver, derr := as[string]("Driver", body["Driver"])
	scope, serr := as[string]("Scope", body["Scope"])
	if derr != nil || serr != nil || !oneOf(driver, "", "bridge") || !oneOf(scope, "", "local") {
		g.deny(w, r, http.StatusForbidden, "only local bridge networks are allowed")
		return
	}
	name, err := as[string]("Name", body["Name"])
	if err == nil && !randomName().MatchString(name) {
		err = g.checkNewName("network", name)
	}
	if err == nil {
		err = g.checkProjectLabel(body["Labels"])
	}
	if err == nil {
		err = g.checkIPAM(body["IPAM"])
	}
	if err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	g.label(body)
	status, out := g.relay(w, r, body)
	var created struct{ Id string }
	if status == http.StatusCreated && json.Unmarshal(out, &created) == nil {
		// The task's own container joins, so it reaches the services by name.
		if agent := g.agentID(); agent != "" {
			if s, msg, err := g.call("POST", "/networks/"+url.PathEscape(created.Id)+"/connect", map[string]any{"Container": agent}); err != nil || s != http.StatusOK {
				g.logf("note  could not join %s to the new network: %v %s", g.Agent, err, msg)
			}
		}
	}
}

// EndSession starts, detached in the task's own container (by the ID its
// labels were checked on), EndSessionCmd for session: once the session's
// Claude has exited, what it left running there is ended; while it runs,
// nothing. The gate calls it for each SessionEnd hook of the task for
// the session (see hook.TaskServer.OnSessionEnd). The exec runs as the
// container's user and is not the task's (it is not in g.execs).
func (g *Gate) EndSession(session int64) error {
	g.init()
	agent := g.agentID()
	if agent == "" {
		return errors.New("the task's own container is not there")
	}
	status, out, err := g.call("POST", "/containers/"+agent+"/exec", map[string]any{"Cmd": EndSessionCmd(session)})
	if err != nil || status != http.StatusCreated {
		return fmt.Errorf("exec create: %v %d %s", err, status, out)
	}
	var created struct{ Id string }
	if err := json.Unmarshal(out, &created); err != nil || created.Id == "" || strings.ContainsAny(created.Id, "/?#%") {
		return fmt.Errorf("exec create: unexpected answer %s", out)
	}
	status, out, err = g.call("POST", "/exec/"+created.Id+"/start", map[string]any{"Detach": true})
	if err != nil || status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("exec start: %v %d %s", err, status, out)
	}
	return nil
}

// agentID returns the full ID of the task's own container, found by its
// name and checked by its labels (see owner), or "" if there is none.
func (g *Gate) agentID() string {
	if g.Agent == "" {
		return ""
	}
	who, id, err := g.owner("containers", g.Agent)
	if err != nil || who != "agent" {
		return ""
	}
	return id
}

func gateNetworkDelete(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	id, ok := g.checkOwned(w, r, "networks", m[1])
	if !ok {
		return
	}
	if agent := g.agentID(); agent != "" {
		g.call("POST", "/networks/"+id+"/disconnect", map[string]any{"Container": agent, "Force": true})
	}
	setPath(r, "/networks/"+id)
	g.forward(w, r, nil)
}

// gateNetworkConnect connects (or disconnects) a container of the task, or
// its own, to a network of the task. Both go to the daemon by the full IDs
// the checks found, and so does the endpoint's NetworkID, which the daemon
// would otherwise prefer to the network in the path.
func gateNetworkConnect(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	err = fieldKeys(body, "Container", "EndpointConfig")
	if ep, ok := body["EndpointConfig"].(map[string]any); ok && err == nil {
		err = fieldKeys(ep, "NetworkID")
	}
	if err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	nid, ok := g.checkOwned(w, r, "networks", m[1])
	if !ok {
		return
	}
	container, _ := body["Container"].(string)
	if container == "" {
		g.deny(w, r, http.StatusForbidden, "no container named")
		return
	}
	_, cid, err := g.owner("containers", container)
	if err != nil {
		g.refuseRef(w, r, "containers", container, err)
		return
	}
	body["Container"] = cid
	if ep, ok := body["EndpointConfig"].(map[string]any); ok {
		ep["NetworkID"] = nid
	} else if !isZero(body["EndpointConfig"]) {
		g.deny(w, r, http.StatusBadRequest, "unreadable endpoint settings")
		return
	}
	setPath(r, "/networks/"+nid+"/"+m[2])
	g.forward(w, r, body)
}

// gateContainerCreate checks and labels a container create. From the
// check of the volumes it names (or its creation of a missing one) until
// the daemon has created the container, no volume delete of the task's
// runs (see Gate.volumes); the body is read before that.
func gateContainerCreate(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	if err := g.checkName(r); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	g.volumes.RLock()
	defer g.volumes.RUnlock()
	if err := g.checkCreate(body); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	g.label(body)
	// A container takes the labels of its image that its own do not set,
	// and the task can build or pull an image with any compose project
	// label; an empty one is no project. (The image a name stands for may
	// change until the daemon reads it: so always.)
	if labels := body["Labels"].(map[string]any); labels[composeProject] == nil {
		labels[composeProject] = ""
	}
	g.forward(w, r, body)
}

// HostConfig settings that are harmless whatever their value. Every other
// setting must be unset (zero) or pass its own check in checkHostConfig, so a
// setting this list does not know is refused.
var hostAllowed = map[string]bool{
	"PortBindings": true, "PublishAllPorts": true, "RestartPolicy": true, "AutoRemove": true,
	"ConsoleSize": true, "Init": true, "ReadonlyRootfs": true, "GroupAdd": true,
	"Memory": true, "MemorySwap": true, "MemoryReservation": true, "MemorySwappiness": true,
	"NanoCpus": true, "CpuShares": true, "CpuPeriod": true, "CpuQuota": true, "CpusetCpus": true,
	"PidsLimit": true, "Ulimits": true, "OomKillDisable": true, "ShmSize": true, "Tmpfs": true,
	"ExtraHosts": true, "Dns": true, "DnsOptions": true, "DnsSearch": true, "CapDrop": true,
}

// createFields are the top-level fields of a container create: the
// container's Config (MacAddress: up to API 1.43), HostConfig and
// NetworkingConfig. Any other is refused unless unset (see checkCreate).
var createFields = []string{
	"Hostname", "Domainname", "User", "AttachStdin", "AttachStdout", "AttachStderr", "ExposedPorts", "Tty",
	"OpenStdin", "StdinOnce", "Env", "Cmd", "Healthcheck", "ArgsEscaped", "Image", "Volumes", "WorkingDir",
	"Entrypoint", "NetworkDisabled", "MacAddress", "OnBuild", "Labels", "StopSignal", "StopTimeout", "Shell",
	"HostConfig", "NetworkingConfig",
}

// hostChecked are the HostConfig settings checkHostConfig checks by value.
var hostChecked = []string{"Binds", "Mounts", "NetworkMode", "VolumesFrom", "LogConfig", "IpcMode", "CgroupnsMode", "MaskedPaths", "ReadonlyPaths"}

// checkCreate checks a container create's body, changing in it what the
// gate forwards otherwise (see checkHostConfig and checkNetworking).
func (g *Gate) checkCreate(body map[string]any) error {
	if err := fieldKeys(body, "HostConfig", "NetworkingConfig", "Labels"); err != nil {
		return err
	}
	if err := g.checkProjectLabel(body["Labels"]); err != nil {
		return err
	}
	// Docker 24 and older also read HostConfig settings at the top level:
	// all of them when there is no HostConfig, a few besides one.
	for k, v := range body {
		known := slices.ContainsFunc(createFields, func(f string) bool { return strings.EqualFold(k, f) })
		paths := strings.EqualFold(k, "MaskedPaths") || strings.EqualFold(k, "ReadonlyPaths")
		if !known && (!isZero(v) || paths && v != nil) {
			return fmt.Errorf("container setting %s is not allowed here (host settings go in HostConfig)", k)
		}
	}
	hc, err := as[map[string]any]("HostConfig", body["HostConfig"])
	if err != nil {
		return err
	}
	if err := g.checkHostConfig(hc); err != nil {
		return err
	}
	nc, err := as[map[string]any]("NetworkingConfig", body["NetworkingConfig"])
	if err != nil {
		return err
	}
	return g.checkNetworking(nc)
}

// checkHostConfig checks a container create's HostConfig: each setting is
// harmless (hostAllowed), passes its own check here or is unset. What the
// gate forwards otherwise (a mount made read-only, objects by full ID) is
// changed in hc.
func (g *Gate) checkHostConfig(hc map[string]any) error {
	if err := fieldKeys(hc, append(slices.Collect(maps.Keys(hostAllowed)), hostChecked...)...); err != nil {
		return err
	}
	for k, v := range hc {
		var err error
		switch k {
		case "Binds":
			err = g.binds(v)
		case "Mounts":
			err = g.mounts(v)
		case "NetworkMode":
			var s string
			if s, err = as[string](k, v); err == nil {
				hc[k], err = g.network(s)
			}
		case "VolumesFrom":
			var list []any
			list, err = as[[]any](k, v)
			for i, c := range list {
				name, ok := c.(string)
				if !ok {
					err = fmt.Errorf("setting %s has the wrong type", k)
					break
				}
				ref, mode, hasMode := strings.Cut(name, ":")
				id, e := g.owned("containers", ref)
				if e != nil {
					err = fmt.Errorf("volumes from %q: %v", ref, e)
					break
				}
				if hasMode {
					id += ":" + mode
				}
				list[i] = id
			}
		case "LogConfig":
			var lc map[string]any
			if lc, err = as[map[string]any](k, v); err != nil {
				break
			}
			if err = fieldKeys(lc, "Type"); err != nil {
				break
			}
			var t string
			if t, err = as[string]("LogConfig.Type", lc["Type"]); err == nil && !oneOf(t, "", "json-file", "local", "none") {
				err = fmt.Errorf("log driver %s is not allowed (json-file, local and none are)", t)
			}
		case "IpcMode":
			var s string
			if s, err = as[string](k, v); err == nil && !oneOf(s, "", "private", "shareable", "none") {
				err = fmt.Errorf("IPC mode %s is not allowed", s)
			}
		case "CgroupnsMode":
			var s string
			if s, err = as[string](k, v); err == nil && !oneOf(s, "", "private") {
				err = fmt.Errorf("cgroup namespace mode %s is not allowed", s)
			}
		case "MaskedPaths", "ReadonlyPaths":
			if v != nil { // an empty list unmasks /proc and /sys
				err = fmt.Errorf("%s may not be changed", k)
			}
		default:
			if !hostAllowed[k] && !isZero(v) {
				err = fmt.Errorf("container setting %s is not allowed", k)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkNetworking checks a container create's NetworkingConfig: each
// network its endpoints name must be one the container may join (see
// network), and is rewritten to its full ID.
func (g *Gate) checkNetworking(nc map[string]any) error {
	if err := fieldKeys(nc, "EndpointsConfig"); err != nil {
		return err
	}
	endpoints, _ := nc["EndpointsConfig"].(map[string]any)
	if nc != nil && endpoints == nil && !isZero(nc["EndpointsConfig"]) {
		return errors.New("unreadable endpoint settings")
	}
	fixed := map[string]any{}
	for name, v := range endpoints {
		ep, ok := v.(map[string]any)
		if !ok && v != nil {
			return errors.New("unreadable endpoint settings")
		}
		if err := fieldKeys(ep, "NetworkID"); err != nil {
			return err
		}
		id, err := g.network(name)
		if err != nil {
			return err
		}
		if id != name {
			// A network of the task's: by its ID, also in the endpoint's
			// NetworkID, which the daemon prefers to the key.
			if ep == nil {
				ep = map[string]any{}
			}
			ep["NetworkID"] = id
		} else if ep != nil {
			delete(ep, "NetworkID") // default, bridge, none: by name only
		}
		if ep != nil {
			fixed[id] = ep
		} else {
			fixed[id] = v
		}
	}
	if endpoints != nil {
		nc["EndpointsConfig"] = fixed
	}
	return nil
}

// network checks a network mode (or a network named in the endpoint
// settings) and returns what to forward instead: a network of the task's,
// or "container:" with a container of the task's (or its own), by full ID;
// the daemon's built-in modes as they are.
func (g *Gate) network(mode string) (string, error) {
	switch {
	case oneOf(mode, "", "default", "bridge", "none"):
		return mode, nil
	case mode == "host":
		return "", errors.New("host networking is not allowed")
	case strings.HasPrefix(mode, "container:"):
		ref := strings.TrimPrefix(mode, "container:")
		_, id, err := g.owner("containers", ref)
		if err != nil {
			return "", fmt.Errorf("network of container %q: %v", ref, err)
		}
		return "container:" + id, nil
	}
	id, err := g.owned("networks", mode)
	if err != nil {
		return "", fmt.Errorf("network %q: %v", mode, err)
	}
	return id, nil
}

func (g *Gate) binds(v any) error {
	list, err := as[[]any]("Binds", v)
	if err != nil {
		return err
	}
	for i, item := range list {
		spec, _ := item.(string)
		parts := strings.Split(spec, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return fmt.Errorf("unreadable mount %q", spec)
		}
		src, ro, err := g.source(parts[0])
		if err != nil {
			return err
		}
		var opts []string
		if len(parts) == 3 {
			opts = strings.Split(parts[2], ",")
			for _, opt := range opts {
				if !oneOf(opt, "ro", "rw", "z", "Z", "nocopy", "cached", "delegated", "consistent") {
					return fmt.Errorf("mount option %s is not allowed", opt)
				}
			}
		}
		parts[0] = src
		if ro {
			parts = append(parts[:2], strings.Join(g.readOnlyBind(src, opts), ","))
		}
		list[i] = strings.Join(parts, ":")
	}
	return nil
}

// readOnlyBind returns the options of a bind of src, a path of ROMounts,
// made read-only: "ro" and none of "rw", nor "z" or "Z", which would have
// the daemon relabel src on the host. A change is logged.
func (g *Gate) readOnlyBind(src string, opts []string) []string {
	var out []string
	asked := false
	for _, opt := range opts {
		switch opt {
		case "ro":
			asked = true
		case "rw":
		case "z", "Z":
			g.logf("note  bind of %s: option %s dropped: a path of $%s is not relabelled", src, opt, MountsVar)
		default:
			out = append(out, opt)
		}
	}
	if !asked || slices.Contains(opts, "rw") {
		g.logf("note  bind of %s made read-only: a path of $%s, which tasks get read-only only", src, MountsVar)
	}
	return append(out, "ro")
}

func (g *Gate) mounts(v any) error {
	list, err := as[[]any]("Mounts", v)
	if err != nil {
		return err
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return errors.New("setting Mounts has the wrong type")
		}
		if err := fieldKeys(m, "Type", "Source", "ReadOnly", "BindOptions", "VolumeOptions"); err != nil {
			return err
		}
		bindOpts, err := as[map[string]any]("Mounts.BindOptions", m["BindOptions"])
		if err != nil {
			return err
		}
		if err := fieldKeys(bindOpts, "Propagation", "ReadOnlyNonRecursive"); err != nil {
			return err
		}
		volumeOpts, err := as[map[string]any]("Mounts.VolumeOptions", m["VolumeOptions"])
		if err != nil {
			return err
		}
		if err := fieldKeys(volumeOpts, "DriverConfig"); err != nil {
			return err
		}
		src, err := as[string]("Mounts.Source", m["Source"])
		if err != nil {
			return err
		}
		switch m["Type"] {
		case "bind":
			p, err := as[string]("Mounts.BindOptions.Propagation", bindOpts["Propagation"])
			if err != nil {
				return err
			}
			if !oneOf(p, "", "private", "rprivate") {
				return fmt.Errorf("mount propagation %s is not allowed", p)
			}
			if !strings.HasPrefix(src, "/") {
				return fmt.Errorf("unreadable mount source %q", src)
			}
			real, ro, err := g.source(src)
			if err != nil {
				return err
			}
			m["Source"] = real
			if ro {
				g.readOnlyMount(real, m)
			}
		case "volume":
			if !isZero(volumeOpts["DriverConfig"]) {
				return errors.New("volume drivers and driver options are not allowed")
			}
			if src != "" {
				_, ro, err := g.source(src)
				if err != nil {
					return err
				}
				if ro { // not a volume name: the daemon refuses it, read-only all the same
					g.readOnlyMount(src, m)
				}
			}
		case "tmpfs":
		default:
			return fmt.Errorf("mount type %v is not allowed", m["Type"])
		}
	}
	return nil
}

// readOnlyMount makes m, an entry of HostConfig.Mounts whose source src is
// a path of ROMounts, read-only, also below (no ReadOnlyNonRecursive). A
// change is logged.
func (g *Gate) readOnlyMount(src string, m map[string]any) {
	if r, _ := m["ReadOnly"].(bool); !r {
		g.logf("note  mount of %s made read-only: a path of $%s, which tasks get read-only only", src, MountsVar)
	}
	m["ReadOnly"] = true
	if opts, _ := m["BindOptions"].(map[string]any); opts != nil {
		delete(opts, "ReadOnlyNonRecursive")
	}
}

// source checks the left side of a mount and returns what to mount instead:
// a volume of this task (made, labelled, if none has the name and the name
// is in the task's namespace), the gate's own socket in place of the Docker socket
// (so nested Docker clients stay gated), or a path inside the task directory
// with its symlinks resolved. A path that resolves to one of ROMounts, or
// into one, is allowed too, if it exists (the daemon would create a missing
// bind source there), with ro true: it must be mounted read-only.
func (g *Gate) source(src string) (path string, ro bool, err error) {
	if !strings.HasPrefix(src, "/") {
		// By name, as the daemon looks it up: exactly, no prefix.
		_, err := g.owned("volumes", src)
		if err == errNotFound { // created on the fly by the daemon otherwise, without the label
			// A new name only in the task's namespace (see
			// gateVolumeCreate); a volume of the task's of any name, as
			// above, stays its to mount.
			if err := g.checkNewName("volume", src); err != nil {
				return "", false, err
			}
			status, out, cerr := g.call("POST", "/volumes/create", map[string]any{"Name": src, "Labels": map[string]string{LabelTask: g.ID}})
			if cerr != nil || status != http.StatusCreated {
				return "", false, fmt.Errorf("cannot create volume %s: %v %s", src, cerr, out)
			}
			return src, false, nil
		}
		return src, false, err
	}
	if oneOf(filepath.Clean(src), "/var/run/docker.sock", "/run/docker.sock") {
		return g.SocketHost, false, nil
	}
	real, err := resolveExisting(src)
	if err != nil {
		return "", false, err
	}
	if real == g.Root || strings.HasPrefix(real, g.Root+"/") {
		return real, false, nil
	}
	for _, m := range g.ROMounts {
		if real != m && !within(real, m) {
			continue
		}
		if _, err := os.Lstat(real); err != nil {
			return "", false, fmt.Errorf("mount source %s does not exist", src)
		}
		return real, true, nil
	}
	if len(g.ROMounts) > 0 {
		return "", false, fmt.Errorf("mount source %s is outside the task directory and the paths of $%s", src, MountsVar)
	}
	return "", false, fmt.Errorf("mount source %s is outside the task directory", src)
}

// resolveExisting follows the symlinks of the part of path that exists.
func resolveExisting(path string) (string, error) {
	path = filepath.Clean(path)
	rest := ""
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !os.IsNotExist(err) || path == "/" {
			return "", fmt.Errorf("cannot resolve mount source: %v", err)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = filepath.Dir(path)
	}
}

func oneOf(s string, options ...string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

// as returns v, the value of the setting name the gate checks, as the type
// the gate reads it as; the zero value if it is unset (null or absent). A
// value of another type would pass the check unread: dockerd would refuse
// it, but the gate does not count on that.
func as[T any](name string, v any) (T, error) {
	var t T
	if v == nil {
		return t, nil
	}
	t, ok := v.(T)
	if !ok {
		return t, fmt.Errorf("setting %s has the wrong type", name)
	}
	return t, nil
}

// isZero reports whether a decoded JSON value is unset: null, false, 0, "",
// or an empty list or object. An object with a key is set, whatever its
// value: the settings that are maps (sysctls, annotations, driver options)
// act on a key with an empty value too (it overrides the daemon's default
// sysctl; containerd's shim groups containers by an annotation whatever its
// value).
func isZero(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == ""
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == 0
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// anyTrue reports whether one of vs is true as dockerd reads a boolean
// query value: anything but "", "0", "no", "false" and "none", in any case.
func anyTrue(vs []string) bool {
	for _, v := range vs {
		if !oneOf(strings.ToLower(strings.TrimSpace(v)), "", "0", "no", "false", "none") {
			return true
		}
	}
	return false
}

// anyNonEmpty reports whether one of vs is not blank.
func anyNonEmpty(vs []string) bool {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}
