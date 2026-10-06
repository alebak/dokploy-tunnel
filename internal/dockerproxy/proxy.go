// Package dockerproxy is a least-privilege proxy for the Docker Engine
// API. It holds the Docker socket in place of the companion and forwards
// only the calls the companion makes, with their parameters and bodies
// checked against the exact shape the companion sends: reading containers,
// networks, volumes and Swarm services and tasks, and creating, starting,
// running socat in, and removing repeater containers. Everything else is
// refused with 403.
//
// Whoever reaches the proxy can still read every container's
// configuration, including environment variables, and run repeaters that
// connect to any address on the networks they join. It must therefore be
// reachable by the companion only.
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

const (
	// maxBodyBytes bounds a request body; the companion's are under 2 KiB.
	maxBodyBytes = 64 << 10
	// maxResponseBytes bounds a response the proxy reads itself.
	maxResponseBytes = 8 << 20
	// handshakeTimeout bounds starting an exec's stream.
	handshakeTimeout = 30 * time.Second
	// lingerTimeout bounds how long a stream whose command has exited
	// waits for the client to close its side.
	lingerTimeout = 30 * time.Second
	// execTTL is how long an exec created through the proxy may wait to
	// be started.
	execTTL = time.Minute
	// maxPendingExecs bounds the execs created and not yet started.
	maxPendingExecs = 4096
)

var (
	// versionPrefix matches an API path with its version prefix.
	versionPrefix = regexp.MustCompile(`^(/v[0-9]+\.[0-9]+)(/.*)$`)
	// objectRef matches the IDs and names of containers, networks,
	// volumes, services, tasks and execs.
	objectRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

// hopHeaders are not forwarded from the daemon's responses.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// Proxy forwards the allowed Docker API calls to one daemon. It is safe for
// concurrent use.
type Proxy struct {
	// image is the only image repeaters may run.
	image string
	// pullName and pullTag are the image's pull parameters.
	pullName, pullTag string
	dial              func(ctx context.Context) (net.Conn, error)
	http              *http.Client
	log               *slog.Logger
	now               func() time.Time

	mu sync.Mutex
	// execs are the execs created through the proxy and not started yet,
	// with when they were created.
	execs map[string]time.Time
}

// New returns a proxy to the daemon at dockerHost, in the form
// docker.New takes, that lets repeaters run repeaterImage only.
func New(dockerHost, repeaterImage string, log *slog.Logger) (*Proxy, error) {
	network, addr, err := docker.ParseHost(dockerHost)
	if err != nil {
		return nil, err
	}
	if repeaterImage == "" {
		return nil, errors.New("no repeater image")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		MaxIdleConns:    8,
		IdleConnTimeout: 30 * time.Second,
		// Bodies pass through as the daemon sent them.
		DisableCompression: true,
	}
	name, tag := docker.SplitReference(repeaterImage)
	return &Proxy{
		image: repeaterImage, pullName: name, pullTag: tag,
		dial: dial, http: &http.Client{Transport: transport}, log: log, now: time.Now,
		execs: map[string]time.Time{},
	}, nil
}

// denial is a refused request; its message is returned to the client.
type denial struct{ reason string }

func (d *denial) Error() string { return d.reason }

func deny(format string, args ...any) error {
	return &denial{reason: fmt.Sprintf(format, args...)}
}

// ServeHTTP forwards r if the allow-list lets it through, and answers 403
// otherwise.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := p.route(w, r)
	var d *denial
	switch {
	case errors.As(err, &d):
		p.log.Warn("request denied", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery, "reason", d.reason, "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "doktunnel-socket-proxy: "+r.Method+" "+r.URL.Path+" denied: "+d.reason)
	case err != nil:
		p.log.Error("forwarding failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadGateway, "doktunnel-socket-proxy: "+err.Error())
	}
}

// route checks r against the allow-list and forwards it. It returns a
// *denial for a refused request, and other errors when the daemon could
// not be reached; it has answered the client otherwise.
func (p *Proxy) route(w http.ResponseWriter, r *http.Request) error {
	// Legitimate IDs and names never need escaping.
	if r.URL.RawPath != "" {
		return deny("escaped path")
	}
	prefix, path := "", r.URL.Path
	if m := versionPrefix.FindStringSubmatch(path); m != nil {
		prefix, path = m[1], m[2]
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		if !objectRef.MatchString(part) && path != "/_ping" {
			return deny("not on the allow-list")
		}
	}
	q := r.URL.Query()
	method := r.Method
	get := method == http.MethodGet

	switch {
	case (get || method == http.MethodHead) && path == "/_ping",
		get && path == "/info":
		return p.forwardQuery(w, r, prefix+path, q)
	case get && path == "/containers/json":
		if err := checkListContainers(q); err != nil {
			return err
		}
		return p.forward(w, r, method, prefix+path, q, nil)
	case get && path == "/tasks":
		if err := checkListTasks(q); err != nil {
			return err
		}
		return p.forward(w, r, method, prefix+path, q, nil)
	case method == http.MethodPost && path == "/containers/create":
		return p.createContainer(w, r, prefix, q)
	case method == http.MethodPost && path == "/images/create":
		if err := p.checkPull(q); err != nil {
			return err
		}
		return p.forward(w, r, method, prefix+path, q, nil)
	case get && len(parts) == 3 && parts[0] == "containers" && parts[2] == "json",
		get && len(parts) == 2 && parts[0] == "networks",
		get && len(parts) == 2 && parts[0] == "volumes",
		get && len(parts) == 2 && parts[0] == "services",
		get && len(parts) == 2 && parts[0] == "tasks":
		return p.forwardQuery(w, r, prefix+path, q)
	case method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
		if len(q) > 0 {
			return deny("unexpected query")
		}
		id, err := p.repeater(w, r, prefix, parts[1])
		if id == "" || err != nil {
			return err
		}
		return p.forward(w, r, method, prefix+"/containers/"+url.PathEscape(id)+"/start", nil, nil)
	case method == http.MethodDelete && len(parts) == 2 && parts[0] == "containers":
		if err := checkRemove(q); err != nil {
			return err
		}
		id, err := p.repeater(w, r, prefix, parts[1])
		if id == "" || err != nil {
			return err
		}
		return p.forward(w, r, method, prefix+"/containers/"+url.PathEscape(id), q, nil)
	case method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec":
		return p.createExec(w, r, prefix, parts[1], q)
	case method == http.MethodPost && len(parts) == 3 && parts[0] == "exec" && parts[2] == "start":
		return p.startExec(w, r, prefix, parts[1], q)
	}
	return deny("not on the allow-list")
}

// forwardQuery forwards a read that takes no parameters.
func (p *Proxy) forwardQuery(w http.ResponseWriter, r *http.Request, path string, q url.Values) error {
	if len(q) > 0 {
		return deny("unexpected query")
	}
	return p.forward(w, r, r.Method, path, nil, nil)
}

// checkListContainers allows listing with "all" and label filters only.
func checkListContainers(q url.Values) error {
	if err := onlyKeys(q, "all", "filters"); err != nil {
		return err
	}
	if v, ok := q["all"]; ok && !isBool(v[0]) {
		return deny("malformed all")
	}
	return checkFilters(q, "label")
}

// checkListTasks allows listing with service and desired-state filters
// only.
func checkListTasks(q url.Values) error {
	if err := onlyKeys(q, "filters"); err != nil {
		return err
	}
	return checkFilters(q, "service", "desired-state")
}

// checkFilters checks that the filters parameter, if any, is a JSON object
// of string lists keyed by allowed names only.
func checkFilters(q url.Values, allowed ...string) error {
	raw, ok := q["filters"]
	if !ok {
		return nil
	}
	var filters map[string][]string
	if err := strictDecode([]byte(raw[0]), &filters); err != nil {
		return deny("malformed filters")
	}
	for key := range filters {
		if !contains(allowed, key) {
			return deny("filter %q not allowed", key)
		}
	}
	return nil
}

// checkRemove allows removal with "force" only, never with volumes.
func checkRemove(q url.Values) error {
	if err := onlyKeys(q, "force"); err != nil {
		return err
	}
	if v, ok := q["force"]; ok && !isBool(v[0]) {
		return deny("malformed force")
	}
	return nil
}

// checkPull allows pulling the repeater image only.
func (p *Proxy) checkPull(q url.Values) error {
	if err := onlyKeys(q, "fromImage", "tag"); err != nil {
		return err
	}
	if q.Get("fromImage") != p.pullName || q.Get("tag") != p.pullTag {
		return deny("only the repeater image %s may be pulled", p.image)
	}
	return nil
}

// onlyKeys checks that q holds allowed keys only, each once.
func onlyKeys(q url.Values, allowed ...string) error {
	for key, values := range q {
		if !contains(allowed, key) {
			return deny("query parameter %q not allowed", key)
		}
		if len(values) != 1 {
			return deny("query parameter %q repeated", key)
		}
	}
	return nil
}

func isBool(v string) bool {
	switch v {
	case "0", "1", "true", "false":
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readBody reads r's JSON body, bounded, and strictly decodes it into v.
func readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading the request body: %w", err)
	}
	if len(b) > maxBodyBytes {
		return deny("body too large")
	}
	if err := strictDecode(b, v); err != nil {
		return deny("malformed body: %v", err)
	}
	return nil
}

// strictDecode decodes exactly one JSON value from b into v, rejecting
// unknown fields at every level.
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// createContainer forwards the creation of a repeater, re-encoding the
// checked configuration.
func (p *Proxy) createContainer(w http.ResponseWriter, r *http.Request, prefix string, q url.Values) error {
	if err := onlyKeys(q, "name"); err != nil {
		return err
	}
	if name := q.Get("name"); !isRepeaterName(name) {
		return deny("container name %q is not a repeater's", name)
	}
	var cfg docker.ContainerConfig
	if err := readBody(r, &cfg); err != nil {
		return err
	}
	if err := checkRepeater(cfg, p.image); err != nil {
		return err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return p.forward(w, r, r.Method, prefix+"/containers/create", q, body)
}

// createExec forwards an exec of socat in a repeater, and remembers its
// ID so that it, and only it, may be started.
func (p *Proxy) createExec(w http.ResponseWriter, r *http.Request, prefix, ref string, q url.Values) error {
	if len(q) > 0 {
		return deny("unexpected query")
	}
	var cfg execConfig
	if err := readBody(r, &cfg); err != nil {
		return err
	}
	if err := checkExec(cfg); err != nil {
		return err
	}
	if !p.hasRoomForExec() {
		return deny("too many execs waiting to start")
	}
	id, err := p.repeater(w, r, prefix, ref)
	if id == "" || err != nil {
		return err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	resp, err := p.roundTrip(r.Context(), r.Method, prefix+"/containers/"+url.PathEscape(id)+"/exec", nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		relay(w, resp)
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading the daemon's answer: %w", err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(b, &created); err != nil || !objectRef.MatchString(created.ID) {
		return errors.New("the daemon created an exec without a usable ID")
	}
	p.addExec(created.ID)
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
	return nil
}

// hasRoomForExec reports whether another exec may wait to be started,
// forgetting the expired ones.
func (p *Proxy) hasRoomForExec() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, at := range p.execs {
		if now.Sub(at) > execTTL {
			delete(p.execs, id)
		}
	}
	return len(p.execs) < maxPendingExecs
}

func (p *Proxy) addExec(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execs[id] = p.now()
}

// takeExec reports whether id was created through the proxy recently and
// not started yet, and forgets it.
func (p *Proxy) takeExec(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.execs[id]
	delete(p.execs, id)
	return ok && p.now().Sub(at) <= execTTL
}

// repeater inspects the container ref through the daemon and returns its
// ID if it is a repeater running the repeater image. If the daemon answers
// with an error, that answer is relayed to the client and the ID is empty.
func (p *Proxy) repeater(w http.ResponseWriter, r *http.Request, prefix, ref string) (string, error) {
	resp, err := p.roundTrip(r.Context(), http.MethodGet, prefix+"/containers/"+url.PathEscape(ref)+"/json", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		relay(w, resp)
		return "", nil
	}
	var c docker.Container
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&c); err != nil {
		return "", fmt.Errorf("decoding container %s: %w", ref, err)
	}
	if c.Config.Labels[labelRepeater] != "1" || c.Config.Image != p.image || !isRepeaterName(strings.TrimPrefix(c.Name, "/")) {
		return "", deny("container %s is not a repeater", ref)
	}
	if !objectRef.MatchString(c.ID) {
		return "", fmt.Errorf("the daemon reports container %s with ID %q", ref, c.ID)
	}
	return c.ID, nil
}

// roundTrip sends a request to the daemon; body, when not nil, is JSON.
func (p *Proxy) roundTrip(ctx context.Context, method, path string, q url.Values, body []byte) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking the Docker daemon: %w", err)
	}
	return resp, nil
}

// forward sends a request to the daemon and relays its answer.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, method, path string, q url.Values, body []byte) error {
	resp, err := p.roundTrip(r.Context(), method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	p.log.Debug("forwarded", "method", method, "path", path, "status", resp.StatusCode)
	relay(w, resp)
	return nil
}

// relay copies the daemon's answer to the client unchanged, flushing as it
// goes so that progress streams, such as a pull's, arrive as they are
// sent.
func relay(w http.ResponseWriter, resp *http.Response) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
	for _, h := range hopHeaders {
		dst.Del(h)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
