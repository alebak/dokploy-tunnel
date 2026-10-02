package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"sync"
	"text/tabwriter"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
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
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// DefaultPort is null when Dokploy does not define the port, as for
	// applications and compose services.
	DefaultPort *int `json:"default_port"`
	// Warning explains why name or status are unknown; it is omitted
	// otherwise.
	Warning string `json:"warning,omitempty"`
}

// detailConcurrency bounds the <type>.one calls in flight at once, so a
// large organization does not flood the panel.
const detailConcurrency = 6

func newServicesCommand() *Command {
	var project string
	return &Command{
		Name:    "services",
		Summary: "List Dokploy services that can be forwarded",
		Description: "Lists the projects, environments and services the context's API key can see; " +
			"Dokploy decides what a key can see, and doktunnel applies no filter of its own. " +
			"JSON output: `{\"context\":\"<name>\",\"projects\":[...]}`, where each project has `id`, `name` and " +
			"`environments`; each environment has `id`, `name`, `default` and `services`; and each service has " +
			"`id`, `type`, `name`, `status` and `default_port`. `default_port` is null when Dokploy does not " +
			"define it, as for applications and compose services. Names and statuses that the project list leaves out, " +
			"as it does for databases with owner and admin keys, are read from each service; when that fails, " +
			"`name` and `status` are empty and the service has a `warning` string.",
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
	cctx, key, err := env.ResolveContext()
	if err != nil {
		return err
	}
	base, err := dokploy.ParseBaseURL(cctx.URL)
	if err != nil {
		return clierr.Newf(clierr.Internal, "context %q: %v", cctx.Name, err).
			WithHint(fmt.Sprintf("run 'doktunnel context remove %s' and 'doktunnel context add' again", cctx.Name))
	}
	api := env.NewAPI(base, key)
	projects, err := api.Projects(context.Background())
	if err != nil {
		return apiError(cctx.URL, err)
	}
	if project != "" {
		if projects = filterProjects(projects, project); len(projects) == 0 {
			return clierr.Newf(clierr.NotFound, "no project named %q or with that ID in context %q", project, cctx.Name).
				WithHint("run 'doktunnel services' to list the projects the API key can see")
		}
	}

	warnings, err := fillServiceDetails(context.Background(), api, projects)
	if err != nil {
		return fmt.Errorf("reading service details: %w", err)
	}

	out := toServicesJSON(cctx.Name, projects, warnings)
	if env.JSON {
		// Warnings are part of each service, and stderr stays silent.
		return output.WriteJSON(env.Stdout, out)
	}
	writeServiceWarnings(env.Stderr, out)
	if len(out.Projects) == 0 {
		_, err := fmt.Fprintf(env.Stderr, "No projects are visible to the API key of context %q.\n", cctx.Name)
		return err
	}
	return writeServices(env.Stdout, out)
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
	var wg sync.WaitGroup
	sem := make(chan struct{}, detailConcurrency)
	for _, s := range incomplete {
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
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return warnings, nil
}

// detailKey identifies a service across types.
func detailKey(s dokploy.Service) string {
	return string(s.Type) + "/" + s.ID
}

func toServicesJSON(contextName string, projects []dokploy.Project, warnings map[string]string) servicesJSON {
	out := servicesJSON{Context: contextName, Projects: []projectJSON{}}
	for _, p := range projects {
		pj := projectJSON{ID: p.ID, Name: p.Name, Environments: []environmentJSON{}}
		for _, e := range p.Environments {
			ej := environmentJSON{ID: e.ID, Name: e.Name, Default: e.IsDefault, Services: []serviceJSON{}}
			for _, s := range e.Services {
				sj := serviceJSON{ID: s.ID, Type: string(s.Type), Name: s.Name, Status: s.Status, Warning: warnings[detailKey(s)]}
				if port := dokploy.DefaultPort(s.Type); port != dokploy.UnknownPort {
					sj.DefaultPort = &port
				}
				ej.Services = append(ej.Services, sj)
			}
			pj.Environments = append(pj.Environments, ej)
		}
		out.Projects = append(out.Projects, pj)
	}
	return out
}

// writeServices prints the services grouped by project and environment.
// Unknown values are shown as "-".
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
				fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", s.Type, orDash(s.Name), orDash(s.Status), port, s.ID)
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
