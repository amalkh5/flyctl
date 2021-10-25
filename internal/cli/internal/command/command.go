// Package command implements helpers useful for when building cobra commands.
package command

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/spf13/cobra"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/pkg/iostreams"

	"github.com/superfly/flyctl/docstrings"
	"github.com/superfly/flyctl/internal/client"
	"github.com/superfly/flyctl/internal/logger"
	"github.com/superfly/flyctl/internal/update"

	"github.com/superfly/flyctl/internal/cli/internal/config"
	"github.com/superfly/flyctl/internal/cli/internal/flag"
	"github.com/superfly/flyctl/internal/cli/internal/state"
)

type (
	Preparer func(context.Context) (context.Context, error)

	Runner func(context.Context) error
)

// TODO: remove once all commands are implemented.
var ErrNotImplementedYet = errors.New("command not implemented yet")

func New(usage, short, long string, fn Runner, p ...Preparer) *cobra.Command {
	return &cobra.Command{
		Use:   usage,
		Short: short,
		Long:  long,
		RunE:  newRunE(fn, p...),
	}
}

func FromDocstrings(dsk string, fn Runner, p ...Preparer) *cobra.Command {
	ds := docstrings.Get(dsk)

	return New(ds.Usage, ds.Short, ds.Long, fn, p...)
}

var commonPreparers = []Preparer{
	determineWorkingDir,
	determineUserHomeDir,
	determineConfigDir,
	determineConfigFile,
	loadConfig,
	initClient,
	promptToUpdate,
}

func newRunE(fn Runner, preparers ...Preparer) func(*cobra.Command, []string) error {
	if fn == nil {
		return nil
	}

	return func(cmd *cobra.Command, _ []string) (err error) {
		ctx := cmd.Context()
		ctx = NewContext(ctx, cmd)
		ctx = flag.NewContext(ctx, cmd.Flags())

		// run the common preparers
		if ctx, err = prepare(ctx, commonPreparers...); err != nil {
			return
		}

		// run the preparers specific to the command
		if ctx, err = prepare(ctx, preparers...); err == nil {
			// and run the command
			err = fn(ctx)
		}

		return
	}
}

func prepare(parent context.Context, preparers ...Preparer) (ctx context.Context, err error) {
	ctx = parent

	for _, p := range preparers {
		if ctx, err = p(ctx); err != nil {
			break
		}
	}

	return
}

func promptToUpdate(ctx context.Context) (context.Context, error) {
	update.PromptFor(ctx, iostreams.FromContext(ctx))

	return ctx, nil
}

func determineWorkingDir(ctx context.Context) (context.Context, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("error determining working directory: %w", err)
	}

	logger.FromContext(ctx).
		Debugf("determined working directory: %q", wd)

	return state.WithWorkingDirectory(ctx, wd), nil
}

func determineUserHomeDir(ctx context.Context) (context.Context, error) {
	wd, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("error determining user home directory: %w", err)
	}

	logger.FromContext(ctx).
		Debugf("determined user home directory: %q", wd)

	return state.WithUserHomeDirectory(ctx, wd), nil
}

func determineConfigDir(ctx context.Context) (context.Context, error) {
	dir := filepath.Join(state.UserHomeDirectory(ctx), ".fly")

	logger.FromContext(ctx).
		Debugf("determined config directory: %q", dir)

	return state.WithConfigDirectory(ctx, dir), nil
}

func determineConfigFile(ctx context.Context) (context.Context, error) {
	dir := filepath.Join(state.ConfigDirectory(ctx), "config.yml")

	logger.FromContext(ctx).
		Debugf("determined config file: %q", dir)

	return state.WithConfigFile(ctx, dir), nil
}

func loadConfig(ctx context.Context) (context.Context, error) {
	logger := logger.FromContext(ctx)

	cfg := config.New()

	// first apply the environment
	cfg.ApplyEnv()

	// then the file (if any)
	path := state.ConfigFile(ctx)
	switch err := cfg.ApplyFile(path); {
	case err == nil:
		// config file does not exist exists
	case errors.Is(err, fs.ErrNotExist):
		logger.Warnf("no config file found at %s", path)
	default:
		return nil, err
	}

	// and lastly apply command line flags
	cfg.ApplyFlags(flag.FromContext(ctx))

	logger.Debug("config initialized.")

	return config.NewContext(ctx, cfg), nil
}

func initClient(ctx context.Context) (context.Context, error) {
	logger := logger.FromContext(ctx)
	cfg := config.FromContext(ctx)

	// TODO: refactor so that api package does NOT depend on global state
	api.SetBaseURL(cfg.APIBaseURL)
	api.SetErrorLog(cfg.LogGQLErrors)
	c := client.FromToken(cfg.AccessToken)
	logger.Debug("client initialized.")

	return client.NewContext(ctx, c), nil
}

// RequireSession is a Preparer which makes sure a session exists.
func RequireSession(ctx context.Context) (context.Context, error) {
	if !client.FromContext(ctx).Authenticated() {
		return nil, client.ErrNoAuthToken
	}

	return ctx, nil
}

// RequireOrg is a Preparer which makes sure the user has selected an
// organization. It embeds RequireSession.
func RequireOrg(ctx context.Context) (context.Context, error) {
	ctx, err := RequireSession(ctx)
	if err != nil {
		return nil, err
	}

	client := client.FromContext(ctx).API()

	orgs, err := client.GetOrganizations(nil)
	if err != nil {
		return nil, err
	}
	sort.Slice(orgs[:], func(i, j int) bool { return orgs[i].Type < orgs[j].Type })

	logger := logger.FromContext(ctx)
	slug := config.FromContext(ctx).Organization

	switch {
	case slug == "" && len(orgs) == 1 && orgs[0].Type == "PERSONAL":
		logger.Warnf("Automatically selected %s organization: %s\n",
			strings.ToLower(orgs[0].Type), orgs[0].Name)

		return state.WithOrg(ctx, &orgs[0]), nil
	case slug != "":
		for _, org := range orgs {
			if slug == org.Slug {
				return state.WithOrg(ctx, &org), nil
			}
		}

		return nil, fmt.Errorf(`Organization %q not found`, slug)
	default:
		org, err := selectOrg(orgs)
		if err != nil {
			return nil, err
		}

		return state.WithOrg(ctx, org), nil
	}
}

func selectOrg(orgs []api.Organization) (*api.Organization, error) {
	var options []string
	for _, org := range orgs {
		options = append(options, fmt.Sprintf("%s (%s)", org.Name, org.Slug))
	}

	var selectedOrg int
	prompt := &survey.Select{
		Message:  "Select organization:",
		Options:  options,
		PageSize: 15,
	}

	if err := survey.AskOne(prompt, &selectedOrg); err != nil {
		return nil, err
	}

	return &orgs[selectedOrg], nil
}

func selectOrganization(client *api.Client, slug string, typeFilter *api.OrganizationType) (*api.Organization, error) {
	orgs, err := client.GetOrganizations(typeFilter)
	if err != nil {
		return nil, err
	}

	if len(orgs) == 1 && orgs[0].Type == "PERSONAL" {
		fmt.Printf("Automatically selected %s organization: %s\n", strings.ToLower(orgs[0].Type), orgs[0].Name)
		return &orgs[0], nil
	}

	sort.Slice(orgs[:], func(i, j int) bool { return orgs[i].Type < orgs[j].Type })

	options := []string{}

	for _, org := range orgs {
		options = append(options, fmt.Sprintf("%s (%s)", org.Name, org.Slug))
	}

	selectedOrg := 0
	prompt := &survey.Select{
		Message:  "Select organization:",
		Options:  options,
		PageSize: 15,
	}
	if err := survey.AskOne(prompt, &selectedOrg); err != nil {
		return nil, err
	}

	return &orgs[selectedOrg], nil
}
