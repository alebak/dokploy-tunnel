package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Detailer reads the forwarding details of one service.
type Detailer interface {
	// Details returns the service of type typ with the given ID. It fails
	// with ErrUnauthorized when the key cannot read the service,
	// ErrNotFound when the service does not exist, and ErrUnreachable or
	// ErrUnexpectedResponse as other calls do.
	Details(ctx context.Context, typ ServiceType, id string) (ServiceDetails, error)
}

var _ Detailer = (*Client)(nil)

// rawDetails holds the fields of a <type>.one response that forwarding
// needs. The responses carry much more, including secrets such as
// environment variables and database passwords, which are never decoded.
type rawDetails struct {
	AppName           string  `json:"appName"`
	ServerID          *string `json:"serverId"`
	ComposeType       string  `json:"composeType"`
	ExternalPort      *int    `json:"externalPort"`
	ExternalGRPCPort  *int    `json:"externalGRPCPort"`
	ExternalAdminPort *int    `json:"externalAdminPort"`
	Ports             []struct {
		PublishedPort int    `json:"publishedPort"`
		TargetPort    int    `json:"targetPort"`
		Protocol      string `json:"protocol"`
	} `json:"ports"`
	Domains []struct {
		Host        string  `json:"host"`
		Port        *int    `json:"port"`
		ServiceName *string `json:"serviceName"`
	} `json:"domains"`
	NetworkSwarm []struct {
		Target string `json:"Target"`
	} `json:"networkSwarm"`
	NetworkIDs           []string `json:"networkIds"`
	DetachDokployNetwork bool     `json:"detachDokployNetwork"`
	IsolatedDeployment   bool     `json:"isolatedDeployment"`
	ServiceNetworks      []struct {
		ServiceName          string   `json:"serviceName"`
		NetworkIDs           []string `json:"networkIds"`
		DetachDokployNetwork bool     `json:"detachDokployNetwork"`
	} `json:"serviceNetworks"`
}

// Details implements Detailer with <type>.one, such as postgres.one.
func (c *Client) Details(ctx context.Context, typ ServiceType, id string) (ServiceDetails, error) {
	spec, ok := specFor(typ)
	if !ok {
		return ServiceDetails{}, fmt.Errorf("unknown Dokploy service type %q", typ)
	}
	if id == "" {
		return ServiceDetails{}, fmt.Errorf("empty %s ID", typ)
	}
	procedure := string(typ) + ".one"

	var body json.RawMessage
	if err := c.query(ctx, procedure, url.Values{spec.idField: {id}}, &body); err != nil {
		return ServiceDetails{}, err
	}
	d, err := decodeDetails(spec, id, body)
	if err != nil {
		return ServiceDetails{}, fmt.Errorf("%w: %s for %q: %v", ErrUnexpectedResponse, procedure, id, err)
	}
	return d, nil
}

// decodeDetails decodes and validates the <type>.one response for the
// service id.
func decodeDetails(spec serviceSpec, id string, body json.RawMessage) (ServiceDetails, error) {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return ServiceDetails{}, fmt.Errorf("no service returned")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ServiceDetails{}, err
	}
	var raw rawDetails
	if err := json.Unmarshal(body, &raw); err != nil {
		return ServiceDetails{}, err
	}

	d := ServiceDetails{Service: Service{Type: spec.typ}, AppName: raw.AppName, ComposeType: raw.ComposeType}
	for field, dst := range map[string]*string{spec.idField: &d.ID, "name": &d.Name, spec.statusField: &d.Status} {
		if err := decodeField(fields, field, dst); err != nil {
			return ServiceDetails{}, err
		}
	}
	switch {
	case d.ID != id:
		return ServiceDetails{}, fmt.Errorf("returned service %q instead", d.ID)
	case d.AppName == "":
		return ServiceDetails{}, fmt.Errorf("no appName")
	}
	if raw.ServerID != nil {
		d.ServerID = *raw.ServerID
	}

	ports, err := servicePorts(spec.typ, raw)
	if err != nil {
		return ServiceDetails{}, err
	}
	d.Ports = ports
	d.DefaultPort = UnknownPort
	if isDatabase(spec.typ) {
		d.DefaultPort = ports[0].Target
	}

	for _, rd := range raw.Domains {
		dom := Domain{Host: rd.Host}
		if rd.Port != nil {
			if !validPort(*rd.Port) {
				return ServiceDetails{}, fmt.Errorf("domain %q has invalid port %d", rd.Host, *rd.Port)
			}
			dom.Port = *rd.Port
		}
		if rd.ServiceName != nil {
			dom.ComposeService = *rd.ServiceName
		}
		d.Domains = append(d.Domains, dom)
	}

	d.Networks = Networks{
		NetworkIDs:           nonEmpty(raw.NetworkIDs),
		DetachDokployNetwork: raw.DetachDokployNetwork,
		Isolated:             raw.IsolatedDeployment,
	}
	for _, n := range raw.NetworkSwarm {
		d.Networks.SwarmTargets = append(d.Networks.SwarmTargets, n.Target)
	}
	for _, sn := range raw.ServiceNetworks {
		d.Networks.ComposeServices = append(d.Networks.ComposeServices, ComposeServiceNetworks{
			ServiceName:          sn.ServiceName,
			NetworkIDs:           nonEmpty(sn.NetworkIDs),
			DetachDokployNetwork: sn.DetachDokployNetwork,
		})
	}
	return d, nil
}

// isDatabase reports whether Dokploy deploys typ from a database image that
// listens on a fixed port.
func isDatabase(typ ServiceType) bool {
	return typ != ServiceApplication && typ != ServiceCompose
}

// servicePorts returns the container ports Dokploy configures for a service.
// Databases listen on the fixed ports Dokploy's builders in
// packages/server/src/utils/databases/ map their external ports to;
// applications have a list of port mappings; compose services have none.
func servicePorts(typ ServiceType, raw rawDetails) ([]Port, error) {
	var ports []Port
	switch typ {
	case ServicePostgres:
		ports = []Port{{Target: 5432, Published: deref(raw.ExternalPort), Protocol: "tcp"}}
	case ServiceMySQL, ServiceMariaDB:
		ports = []Port{{Target: 3306, Published: deref(raw.ExternalPort), Protocol: "tcp"}}
	case ServiceMongo:
		ports = []Port{{Target: 27017, Published: deref(raw.ExternalPort), Protocol: "tcp"}}
	case ServiceRedis:
		ports = []Port{{Target: 6379, Published: deref(raw.ExternalPort), Protocol: "tcp"}}
	case ServiceLibSQL:
		// The HTTP port comes first: it is the one libSQL clients use.
		ports = []Port{
			{Name: "http", Target: 8080, Published: deref(raw.ExternalPort), Protocol: "tcp"},
			{Name: "grpc", Target: 5001, Published: deref(raw.ExternalGRPCPort), Protocol: "tcp"},
			{Name: "admin", Target: 5000, Published: deref(raw.ExternalAdminPort), Protocol: "tcp"},
		}
	case ServiceApplication:
		for _, p := range raw.Ports {
			ports = append(ports, Port{Target: p.TargetPort, Published: p.PublishedPort, Protocol: p.Protocol})
		}
	}
	for _, p := range ports {
		if !validPort(p.Target) || (p.Published != 0 && !validPort(p.Published)) {
			return nil, fmt.Errorf("invalid port mapping %d:%d", p.Published, p.Target)
		}
	}
	return ports, nil
}

// validPort reports whether p is a usable TCP or UDP port number.
func validPort(p int) bool {
	return p >= 1 && p <= 65535
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// nonEmpty returns s, or nil when it is empty, so that an absent list and
// an empty one compare equal.
func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
