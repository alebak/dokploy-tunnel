package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"text/tabwriter"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/config"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/output"
)

// servicesJSON is the stable JSON form of "services".
type servicesJSON struct {
	Context  string        `json:"context"`
	Projects []projectJSON `json:"projects"`
}

type projectJSON struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Environments []environmentJSON `json:"environments"`
}

type environmentJSON struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Default  bool          `json:"default"`
	Services []serviceJSON `json:"services"`
}

type serviceJSON struct {
	// ID is the Dokploy service ID, or "<compose ID>/<service>" for a
	// service inside a compose stack.
	ID   string `json:"id"`
	Type string `json:"type"`
	// Kind tells Dokploy services (kindService) apart from the services
	// inside a compose stack (kindComposeService).
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// DefaultPort is null when Dokploy does not define the port, as for
	// applications, compose stacks and the services inside them.
	DefaultPort *int `json:"default_port"`
	// Parent is the ID of the compose stack a compose service belongs to;
	// it is omitted for Dokploy services.
	Parent string `json:"parent,omitempty"`
	// Service is the name of a compose service in its compose file; it is
	// omitted for Dokploy services.
	Service string `json:"service,omitempty"`
	// Warning explains what is unknown about the service and why; it is
	// omitted otherwise.
	Warning string `json:"warning,omitempty"`
}

// The kinds of services "services" lists, and the type of a service inside
// a compose stack.
const (
	kindService        = "service"
	kindComposeService = "compose_service"
	typeComposeService = "compose_service"
)

// noCachedComposeFile is the warning for a compose stack whose compose file
// Dokploy has not stored on the server yet.
const noCachedComposeFile = "internal services unknown: Dokploy has no compose file cached for it yet; deploy it once"

// detailConcurrency bounds the <type>.one calls in flight at once, so a
// large organization does not flood the panel.
const detailConcurrency = 6

func newServicesCommand() *Command {
	var project string
	return &Command{
		Name:    "services",
		Summary: "List Dokploy services that can be forwarded",
		Description: "Lists the projects, environments and services the context's API key can see, " +
			"including the services inside each compose stack; " +
			"Dokploy decides what a key can see, and doktunnel applies no filter of its own. " +
			"JSON output: `{\"context\":\"<name>\",\"projects\":[...]}`, where each project has `id`, `name` and " +
			"`environments`; each environment has `id`, `name`, `default` and `services`; and each service has " +
			"`id`, `type`, `kind`, `name`, `status` and `default_port`. `kind` is `service` for a Dokploy service and " +
			"`compose_service` for a service inside a compose stack, which follows its stack in the list with type " +
			"`compose_service`, ID `<compose ID>/<service>`, name `<compose name>/<service>`, an empty status, " +
			"`parent` (the compose ID) and `service` (its name in the compose file). `default_port` is null when " +
			"Dokploy does not define it, as for applications, compose stacks and the services inside them. Names and " +
			"statuses that the project list leaves out, as it does for databases with owner and admin keys, are read " +
			"from each service; when that fails, `name` and `status` are empty and the service has a `warning` string. " +
			"A compose stack whose services cannot be read is still listed, with a `warning` string.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&project, "project", "", "show only the project with this `name or ID`")
		},
		Run: func(env *Env, args []string) error {
			if len(args) > 0 {
				return clierr.Newf(clierr.InvalidArgument, "unexpected argument %q", args[0]).
					WithHint("pass a project name or ID with --project")
			}
			return runServices(env, project)
		},
	}
}

func runServices(env *Env, project string) error {
	cat, err := env.loadCatalog(context.Background(), project)
	if err != nil {
		return err
	}
	out := toServicesJSON(cat.context.Name, cat.projects, cat.inside, cat.warnings)
	if env.JSON {
		// Warnings are part of each service, and stderr stays silent.
		return output.WriteJSON(env.Stdout, out)
	}
	writeServiceWarnings(env.Stderr, out)
	if len(out.Projects) == 0 {
		_, err := fmt.Fprintf(env.Stderr, "No projects are visible to the API key of context %q.\n", cat.context.Name)
		return err
	}
	return writeServices(env.Stdout, out)
}

// catalog is what the API key of a context can see: its projects with every
// name and status that could be read, and the services inside each compose
// stack.
type catalog struct {
	context config.Context
	// key is the context's API key.
	key string
	// base is the context's panel URL.
	base     *url.URL
	projects []dokploy.Project
	// inside holds the services inside each compose stack, keyed by
	// compose ID.
	inside map[string][]string
	// warnings explain what could not be read, keyed by detailKey.
	warnings map[string]string
}

// loadCatalog reads the catalog of the resolved context, keeping only the
// projects whose name or ID is project when it is not empty.
func (e *Env) loadCatalog(ctx context.Context, project string) (*catalog, error) {
	cctx, key, err := e.ResolveContext()
	if err != nil {
		return nil, err
	}
	base, err := dokploy.ParseBaseURL(cctx.URL)
	if err != nil {
		return nil, clierr.Newf(clierr.Internal, "context %q: %v", cctx.Name, err).
			WithHint(fmt.Sprintf("run 'doktunnel context remove %s' and 'doktunnel context add' again", cctx.Name))
	}
	api := e.NewAPI(base, key)
	projects, err := api.Projects(ctx)
	if err != nil {
		return nil, apiError(cctx.URL, err)
	}
	if project != "" {
		if projects = filterProjects(projects, project); len(projects) == 0 {
			return nil, clierr.Newf(clierr.NotFound, "no project named %q or with that ID in context %q", project, cctx.Name).
				WithHint("run 'doktunnel services' to list the projects the API key can see")
		}
	}

	warnings, err := fillServiceDetails(ctx, api, projects)
	if err != nil {
		return nil, fmt.Errorf("reading service details: %w", err)
	}
	inside, composeWarnings, err := listComposeServices(ctx, api, projects)
	if err != nil {
		return nil, fmt.Errorf("reading compose services: %w", err)
	}
	for k, w := range composeWarnings {
		if warnings[k] != "" {
			w = warnings[k] + "; " + w
		}
		warnings[k] = w
	}
	return &catalog{context: cctx, key: key, base: base, projects: projects, inside: inside, warnings: warnings}, nil
}

// filterProjects returns the projects whose ID or name is nameOrID. Dokploy
// does not require project names to be unique, so several may match.
func filterProjects(projects []dokploy.Project, nameOrID string) []dokploy.Project {
	var matched []dokploy.Project
	for _, p := range projects {
		if p.ID == nameOrID || p.Name == nameOrID {
			matched = append(matched, p)
		}
	}
	return matched
}

// fillServiceDetails completes, in place, the services that project.all
// lists without a name or status, by reading each one's details with at most
// detailConcurrency calls in flight. A failed call does not fail the list:
// the service keeps its ID and gets a warning, keyed by detailKey. Only
// cancellation of ctx is returned as an error.
func fillServiceDetails(ctx context.Context, d dokploy.Detailer, projects []dokploy.Project) (map[string]string, error) {
	var incomplete []*dokploy.Service
	for i := range projects {
		for j := range projects[i].Environments {
			services := projects[i].Environments[j].Services
			for k := range services {
				if services[k].Name == "" || services[k].Status == "" {
					incomplete = append(incomplete, &services[k])
				}
			}
		}
	}

	warnings := map[string]string{}
	var mu sync.Mutex
	err := forEachBounded(ctx, incomplete, func(s *dokploy.Service) {
		details, err := d.Details(ctx, s.Type, s.ID)
		if err != nil {
			mu.Lock()
			warnings[detailKey(*s)] = fmt.Sprintf("name and status unknown: %v", err)
			mu.Unlock()
			return
		}
		if s.Name == "" {
			s.Name = details.Name
		}
		if s.Status == "" {
			s.Status = details.Status
		}
	})
	if err != nil {
		return nil, err
	}
	return warnings, nil
}

// listComposeServices reads the names of the services inside every compose
// stack in projects, with at most detailConcurrency calls in flight, and
// returns them keyed by compose ID. Dokploy checks the key's access to each
// stack. A failed call does not fail the list: the stack gets a warning,
// keyed by detailKey. Only cancellation of ctx is returned as an error.
func listComposeServices(ctx context.Context, l dokploy.ComposeLister, projects []dokploy.Project) (map[string][]string, map[string]string, error) {
	var composes []dokploy.Service
	for _, p := range projects {
		for _, e := range p.Environments {
			for _, s := range e.Services {
				if s.Type == dokploy.ServiceCompose {
					composes = append(composes, s)
				}
			}
		}
	}

	names := map[string][]string{}
	warnings := map[string]string{}
	var mu sync.Mutex
	err := forEachBounded(ctx, composes, func(s dokploy.Service) {
		services, err := l.ComposeServices(ctx, s.ID)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case errors.Is(err, dokploy.ErrNotFound):
			warnings[detailKey(s)] = noCachedComposeFile
		case err != nil:
			warnings[detailKey(s)] = fmt.Sprintf("internal services unknown: %v", err)
		default:
			names[s.ID] = services
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return names, warnings, nil
}

// forEachBounded calls fn for each item, each in its own goroutine, with at
// most detailConcurrency calls running at once. Once ctx is canceled it
// starts no more calls, waits for the running ones and returns ctx.Err().
func forEachBounded[T any](ctx context.Context, items []T, fn func(T)) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, detailConcurrency)
	for _, item := range items {
		// Checked first: a select would pick at random when both are ready.
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(item)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// detailKey identifies a service across types.
func detailKey(s dokploy.Service) string {
	return string(s.Type) + "/" + s.ID
}

// toServicesJSON builds the JSON form of the projects. inside holds the
// services inside each compose stack, keyed by compose ID; each is listed
// right after its stack.
func toServicesJSON(contextName string, projects []dokploy.Project, inside map[string][]string, warnings map[string]string) servicesJSON {
	out := servicesJSON{Context: contextName, Projects: []projectJSON{}}
	for _, p := range projects {
		pj := projectJSON{ID: p.ID, Name: p.Name, Environments: []environmentJSON{}}
		for _, e := range p.Environments {
			ej := environmentJSON{ID: e.ID, Name: e.Name, Default: e.IsDefault, Services: []serviceJSON{}}
			for _, s := range e.Services {
				sj := serviceJSON{ID: s.ID, Type: string(s.Type), Kind: kindService, Name: s.Name, Status: s.Status, Warning: warnings[detailKey(s)]}
				if port := dokploy.DefaultPort(s.Type); port != dokploy.UnknownPort {
					sj.DefaultPort = &port
				}
				ej.Services = append(ej.Services, sj)
				if s.Type != dokploy.ServiceCompose {
					continue
				}
				stack := s.Name
				if stack == "" {
					stack = s.ID
				}
				// Ports of compose services are not known from the API.
				for _, name := range inside[s.ID] {
					ej.Services = append(ej.Services, serviceJSON{
						ID:      s.ID + "/" + name,
						Type:    typeComposeService,
						Kind:    kindComposeService,
						Name:    stack + "/" + name,
						Parent:  s.ID,
						Service: name,
					})
				}
			}
			pj.Environments = append(pj.Environments, ej)
		}
		out.Projects = append(out.Projects, pj)
	}
	return out
}

// writeServices prints the services grouped by project and environment, the
// services inside a compose stack indented under it. Unknown values are
// shown as "-".
func writeServices(w io.Writer, out servicesJSON) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, p := range out.Projects {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintf(tw, "%s (%s)\n", p.Name, p.ID)
		if len(p.Environments) == 0 {
			fmt.Fprintln(tw, "  no environments")
		}
		for _, e := range p.Environments {
			heading := e.Name
			if e.Default {
				heading += " (default)"
			}
			fmt.Fprintf(tw, "  %s\n", heading)
			if len(e.Services) == 0 {
				fmt.Fprintln(tw, "    no services")
				continue
			}
			fmt.Fprintln(tw, "    TYPE\tNAME\tSTATUS\tPORT\tID")
			for _, s := range e.Services {
				port := "-"
				if s.DefaultPort != nil {
					port = strconv.Itoa(*s.DefaultPort)
				}
				typ := s.Type
				if s.Kind == kindComposeService {
					typ = "  " + typ
				}
				fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", typ, orDash(s.Name), orDash(s.Status), port, s.ID)
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing services: %w", err)
	}
	return nil
}

// writeServiceWarnings prints the warning of every service that has one.
func writeServiceWarnings(w io.Writer, out servicesJSON) {
	for _, p := range out.Projects {
		for _, e := range p.Environments {
			for _, s := range e.Services {
				if s.Warning != "" {
					fmt.Fprintf(w, "warning: %s %s: %s\n", s.Type, s.ID, s.Warning)
				}
			}
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
