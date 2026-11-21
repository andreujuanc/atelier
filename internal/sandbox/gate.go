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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Labels the gate puts on Docker objects. Their value is the sandbox ID.
const (
	LabelTask  = "atelier.task"  // containers, networks and volumes a task created
	LabelAgent = "atelier.agent" // the task's own container, created by atl
)

// Gate is a task's Docker socket. It forwards to the host daemon only what a
// task may do: everything the task creates is labelled, the task sees and
// touches only what carries its label, mounts must come from the task's own
// directory, and every container setting that reaches into the host is
// refused. A call that is not in gateRoutes is refused.
//
// Known gap: a task can still swap a mount source for a symlink between the
// gate's check and the daemon's mount. Closing it needs the task directory on
// a volume, with mounts rewritten to volume subpaths the daemon resolves.
// BuildKit sessions are not gated either, so builds need DOCKER_BUILDKIT=0.
type Gate struct {
	ID         string      // sandbox ID, the value of the labels
	Root       string      // task directory: the only place mounts may come from
	SocketHost string      // the gate's socket as the host daemon sees it; a mount of the Docker socket becomes a mount of this
	Agent      string      // the task's own container: joined to every network the task creates
	Upstream   string      // host daemon socket
	Log        *log.Logger // refusals (and every call if Verbose); nil: discarded
	Verbose    bool
	Ident      string // which task and atl binary this gate serves (see Ident); Serve writes it to the run dir

	once   sync.Once
	client *http.Client
	proxy  *httputil.ReverseProxy
	execs  sync.Map // exec IDs created through this gate
}

type gateHandler func(g *Gate, w http.ResponseWriter, r *http.Request, m []string)

type gateRoute struct {
	methods string
	re      *regexp.Regexp
	h       gateHandler
}

func route(methods, pattern string, h gateHandler) gateRoute {
	return gateRoute{methods, regexp.MustCompile("^" + pattern + "$"), h}
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

	// Images are shared between tasks: reading and pulling is fine, removing is not.
	route("GET", `/images/json`, gatePass),
	route("GET", `/images/(.+)/(json|history)`, gatePass),
	route("GET", `/distribution/(.+)/json`, gatePass),
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
	if strings.Contains(r.URL.EscapedPath(), "%") {
		g.deny(w, r, http.StatusForbidden, "encoded characters in the path are not accepted")
		return
	}
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	for _, rt := range gateRoutes {
		for _, method := range strings.Fields(rt.methods) {
			if method != r.Method {
				continue
			}
			if m := rt.re.FindStringSubmatch(path); m != nil {
				rt.h(g, w, r, m)
				return
			}
		}
	}
	g.deny(w, r, http.StatusForbidden, "this Docker API call is not available to tasks")
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
	go func() { // client to daemon; when the client is done sending, say so and keep reading
		io.Copy(back, client)
		if c, ok := back.(*net.UnixConn); ok {
			c.CloseWrite()
		}
	}()
	io.Copy(conn, fromBack)
}

func decodeBody(r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber() // keep 64-bit numbers intact on the way through
	if err := dec.Decode(&body); err != nil && err != io.EOF {
		return nil, fmt.Errorf("unreadable request body: %v", err)
	}
	return body, nil
}

var errNotFound = errors.New("not found")

// owner returns who a container, network or volume belongs to: "task" if it
// carries this task's label, "agent" if it is the task's own container. Any
// other object is an error; errNotFound if it does not exist.
func (g *Gate) owner(kind, id string) (string, error) {
	uri := "/" + kind + "/" + url.PathEscape(id)
	if kind == "containers" {
		uri += "/json"
	}
	status, out, err := g.call("GET", uri, nil)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", errNotFound
	case status != http.StatusOK:
		return "", fmt.Errorf("cannot inspect %s", id)
	}
	var obj struct {
		Labels map[string]string
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return "", err
	}
	labels := obj.Labels
	if kind == "containers" {
		labels = obj.Config.Labels
	}
	switch {
	case labels[LabelTask] == g.ID:
		return "task", nil
	case kind == "containers" && labels[LabelAgent] == g.ID:
		return "agent", nil
	}
	return "", fmt.Errorf("%s %q does not belong to this task", strings.TrimSuffix(kind, "s"), id)
}

// owned reports whether the object carries this task's label.
func (g *Gate) owned(kind, id string) error {
	who, err := g.owner(kind, id)
	if err == nil && who != "task" {
		return fmt.Errorf("%q is the task's own container", id)
	}
	return err
}

// checkOwned answers the request itself when the object is not the task's.
// An object that does not exist is left to the daemon's own "no such ..." reply.
func (g *Gate) checkOwned(w http.ResponseWriter, r *http.Request, kind, id string) bool {
	if err := g.owned(kind, id); err != nil && err != errNotFound {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return false
	}
	return true
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
	who, err := g.owner("containers", m[1])
	switch {
	case err == errNotFound: // the daemon says so itself
	case err != nil:
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	case who == "agent" && !agentActions[action]:
		g.deny(w, r, http.StatusForbidden, "this is the task's own container: it cannot be stopped, removed or renamed from inside")
		return
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
	if !g.checkOwned(w, r, "networks", m[1]) {
		return
	}
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

func gateVolume(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if g.checkOwned(w, r, "volumes", m[1]) {
		g.forward(w, r, nil)
	}
}

func gateExec(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if _, ok := g.execs.Load(m[1]); !ok {
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
func gateExecCreate(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if _, err := g.owner("containers", m[1]); err != nil && err != errNotFound {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !isZero(body["Privileged"]) {
		g.deny(w, r, http.StatusForbidden, "privileged exec is not allowed")
		return
	}
	if status, out := g.relay(w, r, body); status == http.StatusCreated {
		var created struct{ Id string }
		if json.Unmarshal(out, &created) == nil && created.Id != "" {
			g.execs.Store(created.Id, true)
		}
	}
}

func gateImagePull(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	q := r.URL.Query()
	if q.Get("fromImage") == "" || q.Get("fromSrc") != "" {
		g.deny(w, r, http.StatusForbidden, "only pulling from a registry is allowed")
		return
	}
	g.forward(w, r, nil)
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
	default:
		g.forward(w, r, nil)
	}
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
	// The local driver's options can bind any host path (type=none,o=bind,device=/).
	if d, _ := body["Driver"].(string); !oneOf(d, "", "local") || !isZero(body["DriverOpts"]) || !isZero(body["ClusterVolumeSpec"]) {
		g.deny(w, r, http.StatusForbidden, "volume drivers and driver options are not allowed")
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
	for k, v := range body {
		if !networkKeys[k] && !isZero(v) {
			g.deny(w, r, http.StatusForbidden, "network setting "+k+" is not allowed")
			return
		}
	}
	driver, _ := body["Driver"].(string)
	scope, _ := body["Scope"].(string)
	if !oneOf(driver, "", "bridge") || !oneOf(scope, "", "local") {
		g.deny(w, r, http.StatusForbidden, "only local bridge networks are allowed")
		return
	}
	g.label(body)
	status, out := g.relay(w, r, body)
	var created struct{ Id string }
	if status == http.StatusCreated && g.Agent != "" && json.Unmarshal(out, &created) == nil {
		// The task's own container joins, so it reaches the services by name.
		if s, msg, err := g.call("POST", "/networks/"+created.Id+"/connect", map[string]any{"Container": g.Agent}); err != nil || s != http.StatusOK {
			g.logf("note  could not join %s to the new network: %v %s", g.Agent, err, msg)
		}
	}
}

func gateNetworkDelete(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if !g.checkOwned(w, r, "networks", m[1]) {
		return
	}
	if g.Agent != "" {
		g.call("POST", "/networks/"+url.PathEscape(m[1])+"/disconnect", map[string]any{"Container": g.Agent, "Force": true})
	}
	g.forward(w, r, nil)
}

func gateNetworkConnect(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	if !g.checkOwned(w, r, "networks", m[1]) {
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	container, _ := body["Container"].(string)
	if _, err := g.owner("containers", container); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	g.forward(w, r, body)
}

func gateContainerCreate(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	body, err := decodeBody(r)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := g.checkCreate(body); err != nil {
		g.deny(w, r, http.StatusForbidden, err.Error())
		return
	}
	g.label(body)
	g.forward(w, r, body)
}

// HostConfig settings that are harmless whatever their value. Every other
// setting must be unset (zero) or pass its own check in checkCreate, so a
// setting this list does not know is refused.
var hostAllowed = map[string]bool{
	"PortBindings": true, "PublishAllPorts": true, "RestartPolicy": true, "AutoRemove": true,
	"LogConfig": true, "ConsoleSize": true, "Init": true, "ReadonlyRootfs": true, "GroupAdd": true,
	"Memory": true, "MemorySwap": true, "MemoryReservation": true, "MemorySwappiness": true,
	"NanoCpus": true, "CpuShares": true, "CpuPeriod": true, "CpuQuota": true, "CpusetCpus": true,
	"PidsLimit": true, "Ulimits": true, "OomKillDisable": true, "ShmSize": true, "Tmpfs": true,
	"ExtraHosts": true, "Dns": true, "DnsOptions": true, "DnsSearch": true, "CapDrop": true,
}

func (g *Gate) checkCreate(body map[string]any) error {
	hc, _ := body["HostConfig"].(map[string]any)
	for k, v := range hc {
		var err error
		switch k {
		case "Binds":
			err = g.binds(v)
		case "Mounts":
			err = g.mounts(v)
		case "NetworkMode":
			s, _ := v.(string)
			err = g.network(s)
		case "VolumesFrom":
			list, _ := v.([]any)
			for _, c := range list {
				name, _ := c.(string)
				if e := g.owned("containers", strings.SplitN(name, ":", 2)[0]); e != nil {
					err = e
				}
			}
		case "IpcMode":
			if s, _ := v.(string); !oneOf(s, "", "private", "shareable", "none") {
				err = fmt.Errorf("IPC mode %s is not allowed", s)
			}
		case "CgroupnsMode":
			if s, _ := v.(string); !oneOf(s, "", "private") {
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
	nc, _ := body["NetworkingConfig"].(map[string]any)
	endpoints, _ := nc["EndpointsConfig"].(map[string]any)
	for name := range endpoints {
		if err := g.network(name); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gate) network(mode string) error {
	switch {
	case oneOf(mode, "", "default", "bridge", "none"):
		return nil
	case mode == "host":
		return errors.New("host networking is not allowed")
	case strings.HasPrefix(mode, "container:"):
		_, err := g.owner("containers", strings.TrimPrefix(mode, "container:"))
		return err
	}
	if err := g.owned("networks", mode); err != nil {
		return fmt.Errorf("network %q: %v", mode, err)
	}
	return nil
}

func (g *Gate) binds(v any) error {
	list, _ := v.([]any)
	for i, item := range list {
		spec, _ := item.(string)
		parts := strings.Split(spec, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return fmt.Errorf("unreadable mount %q", spec)
		}
		src, err := g.source(parts[0])
		if err != nil {
			return err
		}
		if len(parts) == 3 {
			for _, opt := range strings.Split(parts[2], ",") {
				if !oneOf(opt, "ro", "rw", "z", "Z", "nocopy", "cached", "delegated", "consistent") {
					return fmt.Errorf("mount option %s is not allowed", opt)
				}
			}
		}
		parts[0] = src
		list[i] = strings.Join(parts, ":")
	}
	return nil
}

func (g *Gate) mounts(v any) error {
	list, _ := v.([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		src, _ := m["Source"].(string)
		switch m["Type"] {
		case "bind":
			if opts, _ := m["BindOptions"].(map[string]any); opts != nil {
				if p, _ := opts["Propagation"].(string); !oneOf(p, "", "private", "rprivate") {
					return fmt.Errorf("mount propagation %s is not allowed", p)
				}
			}
			if !strings.HasPrefix(src, "/") {
				return fmt.Errorf("unreadable mount source %q", src)
			}
			real, err := g.source(src)
			if err != nil {
				return err
			}
			m["Source"] = real
		case "volume":
			if opts, _ := m["VolumeOptions"].(map[string]any); opts != nil && !isZero(opts["DriverConfig"]) {
				return errors.New("volume drivers and driver options are not allowed")
			}
			if src != "" {
				if _, err := g.source(src); err != nil {
					return err
				}
			}
		case "tmpfs":
		default:
			return fmt.Errorf("mount type %v is not allowed", m["Type"])
		}
	}
	return nil
}

// source checks the left side of a mount and returns what to mount instead:
// a volume of this task, the gate's own socket in place of the Docker socket
// (so nested Docker clients stay gated), or a path inside the task directory
// with its symlinks resolved.
func (g *Gate) source(src string) (string, error) {
	if !strings.HasPrefix(src, "/") {
		err := g.owned("volumes", src)
		if err == errNotFound { // created on the fly by the daemon otherwise, without the label
			status, out, cerr := g.call("POST", "/volumes/create", map[string]any{"Name": src, "Labels": map[string]string{LabelTask: g.ID}})
			if cerr != nil || status != http.StatusCreated {
				return "", fmt.Errorf("cannot create volume %s: %v %s", src, cerr, out)
			}
			return src, nil
		}
		return src, err
	}
	if oneOf(filepath.Clean(src), "/var/run/docker.sock", "/run/docker.sock") {
		return g.SocketHost, nil
	}
	real, err := resolveExisting(src)
	if err != nil {
		return "", err
	}
	if real != g.Root && !strings.HasPrefix(real, g.Root+"/") {
		return "", fmt.Errorf("mount source %s is outside the task directory", src)
	}
	return real, nil
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

// isZero reports whether a decoded JSON value is unset: null, false, 0, "",
// or a list or object holding nothing but such values.
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
		for _, item := range v {
			if !isZero(item) {
				return false
			}
		}
		return true
	}
	return false
}
