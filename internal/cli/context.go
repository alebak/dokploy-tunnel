package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/config"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/keyring"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
)

// apiKeyEnv is the environment variable that can hold the API key for
// "context add".
const apiKeyEnv = "DOKTUNNEL_API_KEY"

// contextJSON is the stable JSON form of a context.
type contextJSON struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Current          bool   `json:"current"`
	// Warnings is set only by "context add".
	Warnings []string `json:"warnings,omitempty"`
}

func toJSON(c config.Context, current string) contextJSON {
	return contextJSON{
		Name:             c.Name,
		URL:              c.URL,
		OrganizationID:   c.OrganizationID,
		OrganizationName: c.OrganizationName,
		Current:          c.Name == current,
	}
}

func newContextCommand() *Command {
	return &Command{
		Name:    "context",
		Summary: "Manage contexts: the Dokploy servers and credentials to use",
		Description: "A context is one Dokploy panel URL and one organization, authenticated by a Dokploy API key. " +
			"The key is stored only in the OS keyring; the panel URL, alias and organization are stored in the config file.",
		Subcommands: []*Command{
			newContextAddCommand(),
			{
				Name:    "list",
				Summary: "List contexts and mark the current one",
				Description: "JSON output: `{\"current_context\":\"<name or empty>\",\"contexts\":[<context>, ...]}`, where each " +
					"context has the fields `name`, `url`, `organization_id`, `organization_name` and `current`.",
				Run: runContextList,
			},
			{
				Name:        "use",
				Summary:     "Set the current context",
				Description: "JSON output: the selected context, with `current` set to true.",
				Args:        "<name>",
				Run:         runContextUse,
			},
			{
				Name:    "remove",
				Summary: "Remove a context and its API key from the keyring",
				Description: "Removing the current context leaves no context current. " +
					"The API key stays valid in Dokploy; revoke it there if it is no longer needed. " +
					"JSON output: `{\"name\":\"<name>\",\"removed\":true}`.",
				Args: "<name>",
				Run:  runContextRemove,
			},
		},
	}
}

func newContextAddCommand() *Command {
	var rawURL, name string
	var keyFromStdin bool
	return &Command{
		Name:    "add",
		Summary: "Register a Dokploy panel and organization, validating its API key",
		Description: "Create the API key in the Dokploy panel under Settings > API keys, choosing the organization. " +
			"The key is read from a hidden prompt, from stdin with `--api-key-stdin`, or from the `" + apiKeyEnv +
			"` environment variable. It is never accepted as a flag value, because flag values end up in shell history " +
			"and process listings. The first context added becomes the current one. " +
			"JSON output: the context, with the fields `name`, `url`, `organization_id`, `organization_name` and `current`, " +
			"plus a `warnings` list of strings when there are warnings, such as for a plain `http://` URL.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&rawURL, "url", "", "Dokploy panel `URL`, such as https://dokploy.example.com or http://192.168.1.20:3000")
			fs.StringVar(&name, "name", "", "context `name` (alias): letters, digits, '.', '_' or '-'")
			fs.BoolVar(&keyFromStdin, "api-key-stdin", false, "read the API key from the first line of stdin")
		},
		Run: func(env *Env, args []string) error {
			if len(args) > 0 {
				return clierr.Newf(clierr.InvalidArgument, "unexpected argument %q", args[0]).
					WithHint("pass the panel URL with --url and the alias with --name")
			}
			return runContextAdd(env, rawURL, name, keyFromStdin)
		},
	}
}

func runContextAdd(env *Env, rawURL, name string, keyFromStdin bool) error {
	var err error
	if rawURL == "" {
		if rawURL, err = env.Input.Ask(prompt.Request{Flag: "url", Label: "Dokploy panel URL"}); err != nil {
			return err
		}
	}
	base, err := dokploy.ParseBaseURL(rawURL)
	if err != nil {
		return clierr.New(clierr.InvalidArgument, err.Error()).
			WithHint("pass --url with the address of the Dokploy panel, such as https://dokploy.example.com")
	}
	var warnings []string
	if base.Scheme == "http" {
		w := "the panel URL uses plain http://, so the API key and all traffic travel unencrypted; use https:// unless the network is trusted"
		warnings = append(warnings, w)
		// In JSON mode stderr stays silent; the warning is part of the result.
		if !env.JSON {
			fmt.Fprintf(env.Stderr, "warning: %s\n", w)
		}
	}

	if name == "" {
		if name, err = env.Input.Ask(prompt.Request{Flag: "name", Label: "Context name"}); err != nil {
			return err
		}
	}
	if err := config.ValidateName(name); err != nil {
		return clierr.New(clierr.InvalidArgument, err.Error())
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.Find(name); ok {
		return clierr.Newf(clierr.InvalidArgument, "context %q already exists", name).
			WithHint(fmt.Sprintf("choose another --name, or run 'doktunnel context remove %s' first", name))
	}
	if env.Keyring == nil {
		return errNoKeyring()
	}

	apiKey, err := env.apiKey(keyFromStdin)
	if err != nil {
		return err
	}
	org, err := env.NewAPI(base, apiKey).Organization(context.Background())
	if err != nil {
		return apiError(base.String(), err)
	}

	ctx := config.Context{Name: name, URL: base.String(), OrganizationID: org.ID, OrganizationName: org.Name}
	if err := cfg.Add(ctx); err != nil {
		return fmt.Errorf("adding context: %w", err)
	}
	if cfg.Current == "" {
		cfg.Current = name
	}
	if err := env.Keyring.Set(name, apiKey); err != nil {
		return keyringError(err)
	}
	if err := cfg.Save(env.configPath()); err != nil {
		// Do not leave an orphaned secret behind; the save error is what
		// the user needs to see.
		_ = env.Keyring.Delete(name)
		return err
	}

	out := toJSON(ctx, cfg.Current)
	out.Warnings = warnings
	if env.JSON {
		return output.WriteJSON(env.Stdout, out)
	}
	_, err = fmt.Fprintf(env.Stdout, "Added context %q for organization %q at %s.\n", name, org.Name, ctx.URL)
	if err == nil && out.Current {
		_, err = fmt.Fprintf(env.Stdout, "It is now the current context.\n")
	}
	return err
}

// apiKey returns the API key for "context add" from stdin (--api-key-stdin),
// the environment, or a hidden prompt, in that order.
func (e *Env) apiKey(fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(e.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading the API key from stdin: %w", err)
		}
		key := strings.TrimSpace(line)
		if key == "" {
			return "", clierr.New(clierr.InvalidArgument, "--api-key-stdin was given but stdin has no API key").
				WithHint("pipe the key into the command, for example from a password manager")
		}
		return key, nil
	}
	if key := strings.TrimSpace(e.Getenv(apiKeyEnv)); key != "" {
		return key, nil
	}
	missing := clierr.New(clierr.MissingInput, "missing value for --api-key-stdin: no API key was given").
		WithHint("pipe the key into --api-key-stdin, or set " + apiKeyEnv)
	if e.NoInput || e.ReadSecret == nil {
		return "", missing
	}
	if _, err := fmt.Fprint(e.Stderr, "Dokploy API key (input hidden): "); err != nil {
		return "", fmt.Errorf("writing prompt: %w", err)
	}
	key, err := e.ReadSecret()
	fmt.Fprintln(e.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the API key: %w", err)
	}
	if key = strings.TrimSpace(key); key == "" {
		return "", missing
	}
	return key, nil
}

func runContextList(env *Env, args []string) error {
	if len(args) > 0 {
		return clierr.Newf(clierr.InvalidArgument, "unexpected argument %q", args[0])
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	if env.JSON {
		out := struct {
			CurrentContext string        `json:"current_context"`
			Contexts       []contextJSON `json:"contexts"`
		}{CurrentContext: cfg.Current, Contexts: []contextJSON{}}
		for _, c := range cfg.Contexts {
			out.Contexts = append(out.Contexts, toJSON(c, cfg.Current))
		}
		return output.WriteJSON(env.Stdout, out)
	}
	if len(cfg.Contexts) == 0 {
		_, err := fmt.Fprintln(env.Stderr, "No contexts yet. Add one with 'doktunnel context add'.")
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tURL\tORGANIZATION")
	for _, c := range cfg.Contexts {
		mark := ""
		if c.Name == cfg.Current {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, c.Name, c.URL, c.OrganizationName)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing context list: %w", err)
	}
	return nil
}

func runContextUse(env *Env, args []string) error {
	name, err := contextNameArg("use", args)
	if err != nil {
		return err
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	if err := cfg.Use(name); err != nil {
		return notFoundError(name)
	}
	if err := cfg.Save(env.configPath()); err != nil {
		return err
	}
	ctx, _ := cfg.Find(name)
	if env.JSON {
		return output.WriteJSON(env.Stdout, toJSON(ctx, cfg.Current))
	}
	_, err = fmt.Fprintf(env.Stdout, "Switched to context %q.\n", name)
	return err
}

func runContextRemove(env *Env, args []string) error {
	name, err := contextNameArg("remove", args)
	if err != nil {
		return err
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	wasCurrent := cfg.Current == name
	if err := cfg.Remove(name); err != nil {
		return notFoundError(name)
	}
	if env.Keyring == nil {
		return errNoKeyring()
	}
	// The secret goes first: a context whose secret cannot be deleted stays
	// registered, so the user can retry instead of losing track of the key.
	if err := env.Keyring.Delete(name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return keyringError(err)
	}
	if err := cfg.Save(env.configPath()); err != nil {
		return err
	}
	if env.JSON {
		return output.WriteJSON(env.Stdout, struct {
			Name    string `json:"name"`
			Removed bool   `json:"removed"`
		}{name, true})
	}
	_, err = fmt.Fprintf(env.Stdout, "Removed context %q and its API key from the keyring.\n", name)
	if err == nil && wasCurrent {
		_, err = fmt.Fprintln(env.Stdout, "No context is current now; select one with 'doktunnel context use <name>'.")
	}
	return err
}

// contextNameArg returns the single context name a subcommand takes.
func contextNameArg(sub string, args []string) (string, error) {
	if len(args) != 1 {
		return "", clierr.Newf(clierr.InvalidArgument, "expected exactly one context name, got %d arguments", len(args)).
			WithHint(fmt.Sprintf("usage: doktunnel context %s <name>; run 'doktunnel context list' to see the names", sub))
	}
	return args[0], nil
}

// ResolveContext returns the context selected with --context or, without it,
// the current context, together with its API key from the keyring. Commands
// that talk to Dokploy use it to pick their target.
func (e *Env) ResolveContext() (config.Context, string, error) {
	cfg, err := e.loadConfig()
	if err != nil {
		return config.Context{}, "", err
	}
	ctx, err := cfg.Resolve(e.Context)
	switch {
	case errors.Is(err, config.ErrNoCurrent):
		return config.Context{}, "", clierr.New(clierr.MissingInput, "missing value for --context: no context is current").
			WithHint("pass --context <name>, or select one with 'doktunnel context use <name>'")
	case errors.Is(err, config.ErrNotFound):
		return config.Context{}, "", notFoundError(e.Context)
	case err != nil:
		return config.Context{}, "", err
	}
	if e.Keyring == nil {
		return config.Context{}, "", errNoKeyring()
	}
	key, err := e.Keyring.Get(ctx.Name)
	if errors.Is(err, keyring.ErrNotFound) {
		return config.Context{}, "", clierr.Newf(clierr.PermissionDenied, "no API key is stored for context %q", ctx.Name).
			WithHint(fmt.Sprintf("run 'doktunnel context remove %s' and 'doktunnel context add' again", ctx.Name))
	}
	if err != nil {
		return config.Context{}, "", keyringError(err)
	}
	return ctx, key, nil
}

// configPath returns the config file to use.
func (e *Env) configPath() string {
	if e.ConfigPath != "" {
		return e.ConfigPath
	}
	// DefaultPath only fails when no config directory can be determined;
	// Load and Save then report the empty path.
	path, _ := config.DefaultPath()
	return path
}

// loadConfig reads the config file, mapping failures to CLI errors.
func (e *Env) loadConfig() (*config.Config, error) {
	path := e.ConfigPath
	if path == "" {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return nil, clierr.New(clierr.Internal, err.Error()).
				WithHint("set XDG_CONFIG_HOME (Linux), or make sure your home directory is set")
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		e := clierr.New(clierr.Internal, err.Error())
		if errors.Is(err, config.ErrCorrupt) || errors.Is(err, config.ErrUnsupportedVersion) {
			e = e.WithHint(fmt.Sprintf("fix or move %s and add your contexts again", path))
		}
		return nil, e
	}
	return cfg, nil
}

func notFoundError(name string) error {
	return clierr.Newf(clierr.InvalidArgument, "context %q not found", name).
		WithHint("run 'doktunnel context list' to see the available contexts")
}

func errNoKeyring() error {
	return clierr.New(clierr.Internal, "no OS keyring is available")
}

func keyringError(err error) error {
	return clierr.New(clierr.Internal, err.Error()).
		WithHint("doktunnel stores API keys only in the OS keyring; make sure it is available and unlocked " +
			"(on Linux, a Secret Service provider such as GNOME Keyring or KWallet)")
}

// apiError maps a Dokploy client error to a CLI error.
func apiError(url string, err error) error {
	switch {
	case errors.Is(err, dokploy.ErrUnauthorized):
		return clierr.Newf(clierr.PermissionDenied, "Dokploy at %s rejected the API key: %v", url, err).
			WithHint("create a key in the Dokploy panel under Settings > API keys, choosing the organization, and try again")
	case errors.Is(err, dokploy.ErrUnreachable):
		return clierr.Newf(clierr.Unreachable, "cannot reach Dokploy at %s: %v", url, err).
			WithHint("check the URL, your network, and the HTTPS_PROXY, HTTP_PROXY and NO_PROXY variables")
	case errors.Is(err, dokploy.ErrUnexpectedResponse):
		return clierr.Newf(clierr.Unreachable, "%s did not answer like a Dokploy panel: %v", url, err).
			WithHint("check that --url is the address of the Dokploy panel itself")
	case errors.Is(err, dokploy.ErrNotFound):
		return clierr.Newf(clierr.NotFound, "Dokploy at %s: %v", url, err)
	default:
		return fmt.Errorf("calling Dokploy at %s: %w", url, err)
	}
}
