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

// serviceSpecs lists every service type in the order services are reported.
// The keys mirror the environment relations in Dokploy's
// packages/server/src/db/schema/environment.ts.
var serviceSpecs = []serviceSpec{
	{ServiceApplication, "applications", "applicationId", "applicationStatus"},
	{ServiceCompose, "compose", "composeId", "composeStatus"},
	{ServicePostgres, "postgres", "postgresId", "applicationStatus"},
	{ServiceMySQL, "mysql", "mysqlId", "applicationStatus"},
	{ServiceMariaDB, "mariadb", "mariadbId", "applicationStatus"},
	{ServiceMongo, "mongo", "mongoId", "applicationStatus"},
	{ServiceRedis, "redis", "redisId", "applicationStatus"},
	{ServiceLibSQL, "libsql", "libsqlId", "applicationStatus"},
}

// ServiceTypes returns every service type, in the order services are
// reported.
func ServiceTypes() []ServiceType {
	types := make([]ServiceType, len(serviceSpecs))
	for i, s := range serviceSpecs {
		types[i] = s.typ
	}
	return types
}

// specFor returns how Dokploy names typ, and false for an unknown type.
func specFor(typ ServiceType) (serviceSpec, bool) {
	for _, s := range serviceSpecs {
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
