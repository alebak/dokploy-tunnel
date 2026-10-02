package dokploy

import (
	"context"
	"encoding/json"
	"fmt"
)

// Catalog is the part of the Dokploy API that discovers what can be
// forwarded.
type Catalog interface {
	// Projects lists the projects, environments and services the API key
	// can access. It fails with ErrUnauthorized, ErrUnreachable or
	// ErrUnexpectedResponse.
	Projects(ctx context.Context) ([]Project, error)
}

var _ Catalog = (*Client)(nil)

// Projects implements Catalog with project.all. Dokploy filters the result
// by the key's permissions: owners and admins see everything in the
// organization, members only what they were granted.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var raw []struct {
		ProjectID    string                       `json:"projectId"`
		Name         string                       `json:"name"`
		Environments []map[string]json.RawMessage `json:"environments"`
	}
	if err := c.query(ctx, "project.all", nil, &raw); err != nil {
		return nil, err
	}
	projects := make([]Project, 0, len(raw))
	for _, rp := range raw {
		if rp.ProjectID == "" {
			return nil, fmt.Errorf("%w: project.all listed a project without an ID", ErrUnexpectedResponse)
		}
		p := Project{ID: rp.ProjectID, Name: rp.Name, Environments: make([]Environment, 0, len(rp.Environments))}
		for _, re := range rp.Environments {
			env, err := decodeEnvironment(re)
			if err != nil {
				return nil, fmt.Errorf("%w: project.all, project %q: %v", ErrUnexpectedResponse, p.ID, err)
			}
			p.Environments = append(p.Environments, env)
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// decodeEnvironment decodes one environment of project.all, whose services
// are listed per type under the keys in serviceSpecs.
func decodeEnvironment(fields map[string]json.RawMessage) (Environment, error) {
	var env Environment
	if err := decodeField(fields, "environmentId", &env.ID); err != nil {
		return Environment{}, err
	}
	if env.ID == "" {
		return Environment{}, fmt.Errorf("environment without an ID")
	}
	if err := decodeField(fields, "name", &env.Name); err != nil {
		return Environment{}, err
	}
	if err := decodeField(fields, "isDefault", &env.IsDefault); err != nil {
		return Environment{}, err
	}
	env.Services = []Service{}
	for _, spec := range serviceSpecs() {
		var items []map[string]json.RawMessage
		if err := decodeField(fields, spec.listKey, &items); err != nil {
			return Environment{}, err
		}
		for _, item := range items {
			s := Service{Type: spec.typ}
			for field, dst := range map[string]*string{spec.idField: &s.ID, "name": &s.Name, spec.statusField: &s.Status} {
				if err := decodeField(item, field, dst); err != nil {
					return Environment{}, fmt.Errorf("environment %q: %v", env.ID, err)
				}
			}
			if s.ID == "" {
				return Environment{}, fmt.Errorf("environment %q lists a %s without an ID", env.ID, spec.typ)
			}
			env.Services = append(env.Services, s)
		}
	}
	return env, nil
}

// decodeField decodes fields[name] into dst. A missing or null field leaves
// dst unchanged.
func decodeField(fields map[string]json.RawMessage, name string, dst any) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("field %q: %v", name, err)
	}
	return nil
}
