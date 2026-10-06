// Package dockertest provides a fake Docker Engine API for tests: an
// in-memory daemon that serves the endpoints package docker uses,
// including hijacked exec streams. Tests must never talk to a real daemon.
package dockertest

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

// ExecHandler runs an exec'd command in the fake daemon: it reads stdin,
// writes stdout and stderr, and returns the exit code. stdin ends when the
// client half-closes or closes the stream.
type ExecHandler func(containerID string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int

// Container describes a container to add to the fake daemon.
type Container struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Labels  map[string]string
	// Created defaults to the time the container is added.
	Created time.Time
	// ExposedPorts are like "5432/tcp".
	ExposedPorts []string
	// Networks maps network names to the container's aliases on them. The
	// networks must have been added first.
	Networks map[string][]string
	// IPs maps network names to the container's address there; addresses
	// are assigned for the networks it leaves out.
	IPs map[string]string
	// Privileged, NetworkMode, Binds and Mounts are reported in the
	// container's inspection.
	Privileged  bool
	NetworkMode string
	Binds       []string
	Mounts      []docker.Mount
}

// ExecConfig is an exec the fake daemon was asked to create.
type ExecConfig struct {
	ContainerID  string
	Cmd          []string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
}

// Fake is a fake Docker daemon. Configure it before the code under test
// runs; its accessors are safe for concurrent use.
type Fake struct {
	t   testing.TB
	srv *httptest.Server

	// APIVersion is the version the daemon reports; MaxAPIVersion by
	// default.
	APIVersion string
	// ExecHandler runs exec'd commands; by default they exit at once.
	ExecHandler ExecHandler
	// PullError, when set, makes every pull fail inside its progress
	// stream, as the daemon reports a missing image.
	PullError string
	// CreateError, when set, fails every container creation with this
	// status and message.
	CreateError *docker.APIError
	// Intercept, when set, sees every request first; it returns true when
	// it answered the request itself.
	Intercept func(w http.ResponseWriter, r *http.Request) bool

	mu         sync.Mutex
	containers map[string]*docker.Container
	networks   map[string]docker.Network
	services   map[string]docker.Service
	volumes    map[string]docker.Volume
	tasks      []docker.Task
	nextIP     int
	volumeRMs  int
	images     map[string]bool
	execs      map[string]ExecConfig
	created    []docker.ContainerConfig
	removed    []string
	pulls      []string
	versions   []string
	running    sync.WaitGroup
}

// New starts a fake daemon that is shut down when the test ends.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{
		t:          t,
		APIVersion: docker.MaxAPIVersion,
		containers: map[string]*docker.Container{},
		networks:   map[string]docker.Network{},
		services:   map[string]docker.Service{},
		volumes:    map[string]docker.Volume{},
		images:     map[string]bool{},
		execs:      map[string]ExecConfig{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.srv.Close()
		f.running.Wait()
	})
	return f
}

// Host returns the daemon's address in the form docker.New takes.
func (f *Fake) Host() string {
	return "tcp://" + f.srv.Listener.Addr().String()
}

// AddContainer adds a container.
func (f *Fake) AddContainer(c Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Created.IsZero() {
		c.Created = time.Now()
	}
	dc := &docker.Container{ID: c.ID, Name: "/" + c.Name, Created: c.Created.UTC().Format(time.RFC3339Nano)}
	dc.State.Running = c.Running
	dc.State.Status = map[bool]string{true: "running", false: "exited"}[c.Running]
	if c.Running {
		dc.State.StartedAt = f.startTime("")
	}
	dc.Config.Image = c.Image
	dc.Config.Labels = c.Labels
	if len(c.ExposedPorts) > 0 {
		dc.Config.ExposedPorts = map[string]struct{}{}
		for _, p := range c.ExposedPorts {
			dc.Config.ExposedPorts[p] = struct{}{}
		}
	}
	dc.HostConfig.Privileged = c.Privileged
	dc.HostConfig.NetworkMode = c.NetworkMode
	dc.HostConfig.Binds = c.Binds
	dc.Mounts = c.Mounts
	dc.NetworkSettings.Networks = map[string]docker.EndpointSettings{}
	for name, aliases := range c.Networks {
		n, ok := f.network(name)
		if !ok {
			f.t.Fatalf("dockertest: container %s joins unknown network %s", c.ID, name)
		}
		ip, ok := c.IPs[name]
		if !ok {
			ip = f.assignIP()
		}
		dc.NetworkSettings.Networks[n.Name] = docker.EndpointSettings{NetworkID: n.ID, EndpointID: randomID(), IPAddress: ip, Aliases: aliases, DNSNames: append([]string{c.Name}, aliases...)}
	}
	f.containers[c.ID] = dc
}

// AddNetwork adds a network.
func (f *Fake) AddNetwork(n docker.Network) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[n.ID] = n
}

// AddService adds a Swarm service.
func (f *Fake) AddService(s docker.Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[s.ID] = s
}

// AddVolume adds a volume.
func (f *Fake) AddVolume(v docker.Volume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[v.Name] = v
}

// AddTask adds a Swarm task. Its Status.State is "running" and its
// DesiredState "running" unless set.
func (f *Fake) AddTask(task docker.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if task.DesiredState == "" {
		task.DesiredState = "running"
	}
	if task.Status.State == "" {
		task.Status.State = "running"
	}
	f.tasks = append(f.tasks, task)
}

// assignIP returns a fresh address; f.mu must be held.
func (f *Fake) assignIP() string {
	f.nextIP++
	return fmt.Sprintf("10.99.%d.%d", f.nextIP/250, f.nextIP%250+1)
}

// VolumeRemovals returns how many container removals asked to remove
// volumes too.
func (f *Fake) VolumeRemovals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumeRMs
}

// AddImage makes an image present, as if pulled.
func (f *Fake) AddImage(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[imageKey(ref)] = true
}

// Container returns a container by ID.
func (f *Fake) Container(id string) (docker.Container, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return docker.Container{}, false
	}
	return *c, true
}

// ContainerIDs returns the IDs of every container, sorted.
func (f *Fake) ContainerIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.containers))
	for id := range f.containers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// startTime returns a start time later than prev, an RFC 3339 time or "",
// so that every start of a container is told apart even on coarse clocks.
func (f *Fake) startTime(prev string) string {
	now := time.Now().UTC()
	if p, err := time.Parse(time.RFC3339Nano, prev); err == nil && !now.After(p) {
		now = p.Add(time.Microsecond)
	}
	return now.Format(time.RFC3339Nano)
}

// RestartContainer stops and starts a container again, as the daemon does
// on a restart: it gets a new start time and new network endpoints, and
// keeps its addresses.
func (f *Fake) RestartContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return
	}
	c.State.Running = true
	c.State.Status = "running"
	c.State.StartedAt = f.startTime(c.State.StartedAt)
	for name, ep := range c.NetworkSettings.Networks {
		ep.EndpointID = randomID()
		c.NetworkSettings.Networks[name] = ep
	}
}

// DeleteContainer removes a container, as if something other than the
// code under test had removed it.
func (f *Fake) DeleteContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
}

// SetTaskState sets the Status.State of a Swarm task, such as "shutdown"
// once it has ended.
func (f *Fake) SetTaskState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks[i].Status.State = state
		}
	}
}

// StopContainer marks a container as exited, as if it had died.
func (f *Fake) StopContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.State.Running = false
		c.State.Status = "exited"
	}
}

// Created returns every container configuration created, in order.
func (f *Fake) Created() []docker.ContainerConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.created)
}

// Removed returns the IDs of the containers removed, in order.
func (f *Fake) Removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// Pulls returns the images pulled, as "name:tag" or "name@digest".
func (f *Fake) Pulls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pulls)
}

// Execs returns every exec created, in no particular order.
func (f *Fake) Execs() []ExecConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ExecConfig, 0, len(f.execs))
	for _, e := range f.execs {
		out = append(out, e)
	}
	return out
}

// Versions returns the distinct API versions of versioned requests.
func (f *Fake) Versions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.versions)
}

// network finds a network by ID or name; f.mu must be held.
func (f *Fake) network(idOrName string) (docker.Network, bool) {
	if n, ok := f.networks[idOrName]; ok {
		return n, true
	}
	for _, n := range f.networks {
		if n.Name == idOrName {
			return n, true
		}
	}
	return docker.Network{}, false
}

// container finds a container by ID or name; f.mu must be held.
func (f *Fake) container(idOrName string) (*docker.Container, bool) {
	if c, ok := f.containers[idOrName]; ok {
		return c, true
	}
	for _, c := range f.containers {
		if c.Name == "/"+idOrName {
			return c, true
		}
	}
	return nil, false
}

var versioned = regexp.MustCompile(`^/v([0-9]+\.[0-9]+)(/.*)$`)

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", f.APIVersion)
	if f.Intercept != nil && f.Intercept(w, r) {
		return
	}
	path := r.URL.Path
	if m := versioned.FindStringSubmatch(path); m != nil {
		f.mu.Lock()
		if !slices.Contains(f.versions, m[1]) {
			f.versions = append(f.versions, m[1])
		}
		f.mu.Unlock()
		path = m[2]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/_ping":
		_, _ = io.WriteString(w, "OK")
	case r.Method == http.MethodGet && path == "/containers/json":
		f.listContainers(w, r)
	case r.Method == http.MethodGet && path == "/tasks":
		f.listTasks(w, r)
	case r.Method == http.MethodPost && path == "/containers/create":
		f.createContainer(w, r)
	case r.Method == http.MethodPost && path == "/images/create":
		f.pull(w, r)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json" && r.Method == http.MethodGet:
		f.inspectContainer(w, parts[1])
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "start" && r.Method == http.MethodPost:
		f.startContainer(w, parts[1])
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec" && r.Method == http.MethodPost:
		f.createExec(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "containers" && r.Method == http.MethodDelete:
		f.removeContainer(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "networks" && r.Method == http.MethodGet:
		f.inspectNetwork(w, parts[1])
	case len(parts) == 2 && parts[0] == "services" && r.Method == http.MethodGet:
		f.inspectService(w, parts[1])
	case len(parts) == 2 && parts[0] == "tasks" && r.Method == http.MethodGet:
		f.inspectTask(w, parts[1])
	case len(parts) == 2 && parts[0] == "volumes" && r.Method == http.MethodGet:
		f.inspectVolume(w, parts[1])
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "start" && r.Method == http.MethodPost:
		f.startExec(w, r, parts[1])
	default:
		apiError(w, http.StatusNotFound, "page not found: "+r.Method+" "+path)
	}
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) listContainers(w http.ResponseWriter, r *http.Request) {
	var filters struct {
		Label []string `json:"label"`
	}
	if raw := r.URL.Query().Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			apiError(w, http.StatusBadRequest, "invalid filters: "+err.Error())
			return
		}
	}
	all := r.URL.Query().Get("all") == "1"

	f.mu.Lock()
	defer f.mu.Unlock()
	out := []docker.ContainerSummary{}
	for _, c := range f.containers {
		if (!all && !c.State.Running) || !matchLabels(c.Config.Labels, filters.Label) {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, c.Created)
		out = append(out, docker.ContainerSummary{
			ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels,
			Created: created.Unix(), State: c.State.Status,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func matchLabels(labels map[string]string, filters []string) bool {
	for _, f := range filters {
		k, v, hasValue := strings.Cut(f, "=")
		got, ok := labels[k]
		if !ok || (hasValue && got != v) {
			return false
		}
	}
	return true
}

func (f *Fake) inspectContainer(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(id)
	if !ok {
		apiError(w, http.StatusNotFound, "No such container: "+id)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (f *Fake) createContainer(w http.ResponseWriter, r *http.Request) {
	var cfg docker.ContainerConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateError != nil {
		apiError(w, f.CreateError.StatusCode, f.CreateError.Message)
		return
	}
	if !f.images[imageKey(cfg.Image)] {
		apiError(w, http.StatusNotFound, "No such image: "+cfg.Image)
		return
	}
	name := r.URL.Query().Get("name")
	if _, taken := f.container(name); taken && name != "" {
		apiError(w, http.StatusConflict, "Conflict. The container name \"/"+name+"\" is already in use")
		return
	}
	c := &docker.Container{ID: randomID(), Name: "/" + name, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	c.State.Status = "created"
	c.Config.Image = cfg.Image
	c.Config.Labels = cfg.Labels
	c.NetworkSettings.Networks = map[string]docker.EndpointSettings{}
	if mode := cfg.HostConfig.NetworkMode; mode != "" {
		n, ok := f.network(mode)
		if !ok {
			apiError(w, http.StatusNotFound, "network "+mode+" not found")
			return
		}
		if n.Driver == "overlay" && !n.Attachable {
			apiError(w, http.StatusForbidden, "Could not attach to network "+mode+": network not manually attachable")
			return
		}
		c.NetworkSettings.Networks[n.Name] = docker.EndpointSettings{NetworkID: n.ID, EndpointID: randomID(), IPAddress: f.assignIP()}
	}
	f.containers[c.ID] = c
	f.created = append(f.created, cfg)
	writeJSON(w, http.StatusCreated, map[string]string{"Id": c.ID})
}

func (f *Fake) startContainer(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(id)
	if !ok {
		apiError(w, http.StatusNotFound, "No such container: "+id)
		return
	}
	c.State.Running = true
	c.State.Status = "running"
	c.State.StartedAt = f.startTime(c.State.StartedAt)
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) removeContainer(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := r.URL.Query().Get("v"); v == "1" || v == "true" {
		f.volumeRMs++
	}
	c, ok := f.container(id)
	if !ok {
		apiError(w, http.StatusNotFound, "No such container: "+id)
		return
	}
	delete(f.containers, c.ID)
	f.removed = append(f.removed, c.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) pull(w http.ResponseWriter, r *http.Request) {
	name, tag := r.URL.Query().Get("fromImage"), r.URL.Query().Get("tag")
	ref := name + ":" + tag
	if strings.HasPrefix(tag, "sha256:") {
		ref = name + "@" + tag
	}
	f.mu.Lock()
	f.pulls = append(f.pulls, ref)
	pullErr := f.PullError
	if pullErr == "" {
		f.images[imageKey(ref)] = true
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]string{"status": "Pulling from " + name})
	if pullErr != "" {
		_ = enc.Encode(map[string]any{"error": pullErr, "errorDetail": map[string]string{"message": pullErr}})
		return
	}
	_ = enc.Encode(map[string]string{"status": "Status: Downloaded newer image for " + ref})
}

func (f *Fake) inspectNetwork(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.network(id)
	if !ok {
		apiError(w, http.StatusNotFound, "network "+id+" not found")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (f *Fake) inspectService(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if s.ID == id || s.Spec.Name == id {
			writeJSON(w, http.StatusOK, s)
			return
		}
	}
	apiError(w, http.StatusNotFound, "service "+id+" not found")
}

func (f *Fake) inspectTask(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, task := range f.tasks {
		if task.ID == id {
			writeJSON(w, http.StatusOK, task)
			return
		}
	}
	apiError(w, http.StatusNotFound, "task "+id+" not found")
}

func (f *Fake) inspectVolume(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[name]
	if !ok {
		apiError(w, http.StatusNotFound, "get "+name+": no such volume")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (f *Fake) listTasks(w http.ResponseWriter, r *http.Request) {
	var filters struct {
		Service      []string `json:"service"`
		DesiredState []string `json:"desired-state"`
	}
	if raw := r.URL.Query().Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			apiError(w, http.StatusBadRequest, "invalid filters: "+err.Error())
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []docker.Task{}
	for _, task := range f.tasks {
		if len(filters.Service) > 0 && !slices.ContainsFunc(filters.Service, func(s string) bool { return f.isService(s, task.ServiceID) }) {
			continue
		}
		if len(filters.DesiredState) > 0 && !slices.Contains(filters.DesiredState, task.DesiredState) {
			continue
		}
		out = append(out, task)
	}
	writeJSON(w, http.StatusOK, out)
}

// isService reports whether ref, an ID or name, names service id; f.mu
// must be held.
func (f *Fake) isService(ref, id string) bool {
	if ref == id {
		return true
	}
	s, ok := f.services[id]
	return ok && s.Spec.Name == ref
}

func (f *Fake) createExec(w http.ResponseWriter, r *http.Request, id string) {
	var cfg ExecConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(id)
	if !ok {
		apiError(w, http.StatusNotFound, "No such container: "+id)
		return
	}
	if !c.State.Running {
		apiError(w, http.StatusConflict, "container "+c.ID+" is not running")
		return
	}
	cfg.ContainerID = c.ID
	execID := randomID()
	f.execs[execID] = cfg
	writeJSON(w, http.StatusCreated, map[string]string{"Id": execID})
}

func (f *Fake) startExec(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	cfg, ok := f.execs[id]
	handler := f.ExecHandler
	f.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "No such exec instance: "+id)
		return
	}
	// The body must be consumed before hijacking, or it would be read as
	// stdin.
	_, _ = io.Copy(io.Discard, r.Body)
	if r.Header.Get("Upgrade") != "tcp" {
		apiError(w, http.StatusBadRequest, "the fake only serves upgraded exec streams")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		apiError(w, http.StatusInternalServerError, "cannot hijack")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	f.running.Add(1)
	defer f.running.Done()
	defer conn.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")

	if handler == nil {
		return
	}
	var mu sync.Mutex
	stdout := &frameWriter{mu: &mu, w: conn, stream: 1}
	stderr := &frameWriter{mu: &mu, w: conn, stream: 2}
	handler(cfg.ContainerID, cfg.Cmd, brw.Reader, stdout, stderr)
}

// frameWriter writes one multiplexed-stream frame per Write.
type frameWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	stream byte
}

func (fw *frameWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	var h [8]byte
	h[0] = fw.stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(p)))
	if _, err := fw.w.Write(append(h[:], p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// imageKey identifies an image reference the way the daemon stores it: by
// digest when there is one, else by tag.
func imageKey(ref string) string {
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name = name[:i]
		}
		return name + "@" + digest
	}
	if i := strings.LastIndex(ref, ":"); i <= strings.LastIndex(ref, "/") {
		return ref + ":latest"
	}
	return ref
}

func randomID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// String describes the fake for test failures.
func (f *Fake) String() string {
	return fmt.Sprintf("dockertest.Fake(%s)", f.Host())
}
