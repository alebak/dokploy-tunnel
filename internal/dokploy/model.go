package dokploy

// ServiceType is a kind of Dokploy service. Its value is the name of the
// Dokploy router that manages it.
type ServiceType string

// The service types Dokploy exposes in project.all.
const (
	ServiceApplication ServiceType = "application"
	ServiceCompose     ServiceType = "compose"
	ServicePostgres    ServiceType = "postgres"
	ServiceMySQL       ServiceType = "mysql"
	ServiceMariaDB     ServiceType = "mariadb"
	ServiceMongo       ServiceType = "mongo"
	ServiceRedis       ServiceType = "redis"
	ServiceLibSQL      ServiceType = "libsql"
)

// serviceSpec describes how Dokploy names one service type on the wire.
type serviceSpec struct {
	typ ServiceType
	// listKey is the environment field project.all lists services under.
	listKey string
	// idField names the service ID, both in responses and as the input of
	// <type>.one.
	idField string
	// statusField names the deployment status.
	statusField string
}

// serviceSpecs returns every service type in the order services are reported.
// The keys mirror the environment relations in Dokploy's
// packages/server/src/db/schema/environment.ts.
func serviceSpecs() []serviceSpec {
	return []serviceSpec{
		{ServiceApplication, "applications", "applicationId", "applicationStatus"},
		{ServiceCompose, "compose", "composeId", "composeStatus"},
		{ServicePostgres, "postgres", "postgresId", "applicationStatus"},
		{ServiceMySQL, "mysql", "mysqlId", "applicationStatus"},
		{ServiceMariaDB, "mariadb", "mariadbId", "applicationStatus"},
		{ServiceMongo, "mongo", "mongoId", "applicationStatus"},
		{ServiceRedis, "redis", "redisId", "applicationStatus"},
		{ServiceLibSQL, "libsql", "libsqlId", "applicationStatus"},
	}
}

// ServiceTypes returns every service type, in the order services are
// reported.
func ServiceTypes() []ServiceType {
	specs := serviceSpecs()
	types := make([]ServiceType, len(specs))
	for i, s := range specs {
		types[i] = s.typ
	}
	return types
}

// specFor returns how Dokploy names typ, and false for an unknown type.
func specFor(typ ServiceType) (serviceSpec, bool) {
	for _, s := range serviceSpecs() {
		if s.typ == typ {
			return s, true
		}
	}
	return serviceSpec{}, false
}

// Project is a Dokploy project the API key can see.
type Project struct {
	ID           string
	Name         string
	Environments []Environment
}

// Environment is a deployment environment of a project, such as production.
type Environment struct {
	ID   string
	Name string
	// IsDefault reports the environment Dokploy opens a project with.
	IsDefault bool
	Services  []Service
}

// Service is a service as project.all lists it.
type Service struct {
	ID   string
	Type ServiceType
	// Name is the display name. project.all omits it for database services
	// when the key belongs to an owner or admin; ServiceDetails always has it.
	Name string
	// Status is the deployment status: idle, running, done or error. It is
	// empty when project.all omits it, as it does for Name.
	Status string
}

// ServiceDetails is what forwarding needs to know about one service, read
// from its <type>.one procedure.
type ServiceDetails struct {
	Service
	// AppName is the Docker name Dokploy deploys the service under: the
	// Swarm service name, or the Compose project or Swarm stack name of a
	// compose service.
	AppName string
	// ServerID is the remote server the service runs on, or empty when it
	// runs on the Dokploy server itself.
	ServerID string
	// DefaultPort is the container port forwarding targets by default. It is
	// UnknownPort for applications and compose services, which listen
	// wherever their image does; Ports and Domains hold what Dokploy knows.
	DefaultPort int
	// Ports are the container ports Dokploy configures: the fixed ports of a
	// database, or the port mappings of an application. Compose services
	// declare theirs in the compose file, which this client does not parse.
	Ports []Port
	// Domains are the routes Traefik sends to the service, for applications
	// and compose services.
	Domains []Domain
	// Networks are the Docker networks the service is attached to.
	Networks Networks
	// ComposeType is "docker-compose" or "stack" for compose services, and
	// empty otherwise.
	ComposeType string
}

// UnknownPort is the DefaultPort of a service whose port Dokploy does not
// define.
const UnknownPort = 0

// DefaultPort returns the container port forwarding targets by default for a
// service of type typ: the fixed port Dokploy deploys a database with, or
// UnknownPort for applications, compose services and unknown types. libSQL
// reports its HTTP port, the one libSQL clients use.
func DefaultPort(typ ServiceType) int {
	switch typ {
	case ServicePostgres:
		return 5432
	case ServiceMySQL, ServiceMariaDB:
		return 3306
	case ServiceMongo:
		return 27017
	case ServiceRedis:
		return 6379
	case ServiceLibSQL:
		return 8080
	}
	return UnknownPort
}

// Port is a container port of a service.
type Port struct {
	// Name tells apart the ports of a service that has several, such as
	// libsql's "http", "grpc" and "admin". It is empty otherwise.
	Name string
	// Target is the port inside the container.
	Target int
	// Published is the port Dokploy publishes on the server's host, or 0.
	Published int
	// Protocol is "tcp" or "udp".
	Protocol string
}

// Domain is a host name Traefik routes to a service.
type Domain struct {
	Host string
	// Port is the container port Traefik forwards to, or 0 if unset.
	Port int
	// ComposeService is the compose service the domain targets, for
	// compose services.
	ComposeService string
}

// Networks describes how Dokploy attaches a service to Docker networks.
type Networks struct {
	// SwarmTargets are the networks of a custom Swarm network override.
	// When present, Dokploy attaches the service to exactly these and
	// ignores NetworkIDs and DetachDokployNetwork.
	SwarmTargets []string
	// NetworkIDs are the IDs of the custom Dokploy networks the service is
	// attached to; Dokploy only attaches overlay networks.
	NetworkIDs []string
	// DetachDokployNetwork reports the service is kept off dokploy-network.
	DetachDokployNetwork bool
	// Isolated reports a compose service deployed on its own network
	// instead of dokploy-network.
	Isolated bool
	// ComposeServices are per-service network settings of a compose
	// service.
	ComposeServices []ComposeServiceNetworks
}

// ComposeServiceNetworks are the network settings of one service inside a
// compose file.
type ComposeServiceNetworks struct {
	ServiceName          string
	NetworkIDs           []string
	DetachDokployNetwork bool
}
