package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

// runProject provides the only way to register an arbitrary existing local
// directory. It is intentionally a device-local CLI: remote callers may only
// create a new directory underneath a preconfigured parent, never submit an
// arbitrary absolute path.
func runProject(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: device-agent project <list|add|update|remove> [options]")
	}
	newState := func(flags *flag.FlagSet) (*string, *string) {
		statePath := flags.String("state-file", "", "private device state file")
		workspace := flags.String("workspace", "", "legacy workspace for first-run migration")
		return statePath, workspace
	}
	switch arguments[0] {
	case "list":
		flags := flag.NewFlagSet("project list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath, workspace := newState(flags)
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid project list options")
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		projects, err := state.business.listProjects(context.Background(), false)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(projectCLIViews(projects))
	case "add":
		flags := flag.NewFlagSet("project add", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath, workspace := newState(flags)
		path := flags.String("path", "", "existing local project directory")
		name := flags.String("name", "", "display name")
		gitURL := flags.String("git-url", "", "optional Git URL")
		interactive := flags.Bool("allow-interactive-terminal", true, "allow interactive terminal for this project")
		taskExecution := flags.Bool("allow-task-execution", true, "allow Task v2 runners for this project")
		aiWorkspaceTools := flags.Bool("allow-ai-workspace-tools", true, "allow AI file and command tools for this project")
		remoteCreate := flags.Bool("allow-remote-create", true, "deprecated compatibility option; remote project creation is always enabled")
		recursiveDelete := flags.Bool("allow-recursive-delete", true, "allow confirmed recursive deletion inside this project")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid project add options")
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		project, err := state.business.addProject(context.Background(), *path, *name, *gitURL, projectPolicy{
			AllowInteractiveTerminal: *interactive,
			AllowTaskExecution:       *taskExecution,
			AllowAIWorkspaceTools:    *aiWorkspaceTools,
			AllowRemoteCreate:        *remoteCreate,
			AllowRecursiveDelete:     *recursiveDelete,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(projectCLIView(project))
	case "update":
		flags := flag.NewFlagSet("project update", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath, workspace := newState(flags)
		id := flags.String("id", "", "registered project ID")
		name := flags.String("name", "", "new display name")
		gitURL := flags.String("git-url", "", "new Git URL; use an empty value to clear")
		expected := flags.Uint64("expected-revision", 0, "required current revision (0 disables compare-and-swap)")
		interactive := flags.Bool("allow-interactive-terminal", false, "allow interactive terminal")
		taskExecution := flags.Bool("allow-task-execution", false, "allow Task v2 runners")
		aiWorkspaceTools := flags.Bool("allow-ai-workspace-tools", false, "allow AI file and command tools")
		remoteCreate := flags.Bool("allow-remote-create", false, "deprecated compatibility option; remote project creation is always enabled")
		recursiveDelete := flags.Bool("allow-recursive-delete", false, "allow confirmed recursive deletion")
		setPolicy := flags.Bool("set-policy", false, "update project policy flags")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid project update options")
		}
		projectID, err := uuid.Parse(strings.TrimSpace(*id))
		if err != nil || projectID == uuid.Nil {
			return errors.New("--id must be a project UUID")
		}
		visited := map[string]bool{}
		flags.Visit(func(item *flag.Flag) { visited[item.Name] = true })
		var nameValue, gitValue *string
		if visited["name"] {
			value := *name
			nameValue = &value
		}
		if visited["git-url"] {
			value := *gitURL
			gitValue = &value
		}
		if nameValue == nil && gitValue == nil && !*setPolicy {
			return errors.New("project update requires --name, --git-url, or --set-policy")
		}
		var expectedValue *uint64
		if *expected != 0 {
			value := *expected
			expectedValue = &value
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		var policy *projectPolicy
		if *setPolicy {
			value := projectPolicy{
				AllowInteractiveTerminal: *interactive,
				AllowTaskExecution:       *taskExecution,
				AllowAIWorkspaceTools:    *aiWorkspaceTools,
				AllowRemoteCreate:        *remoteCreate,
				AllowRecursiveDelete:     *recursiveDelete,
			}
			policy = &value
		}
		project, err := state.business.updateProject(context.Background(), projectID, nameValue, gitValue, policy, expectedValue)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(projectCLIView(project))
	case "remove":
		flags := flag.NewFlagSet("project remove", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath, workspace := newState(flags)
		id := flags.String("id", "", "registered project ID")
		expected := flags.Uint64("expected-revision", 0, "required current revision (0 disables compare-and-swap)")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid project remove options")
		}
		projectID, err := uuid.Parse(strings.TrimSpace(*id))
		if err != nil || projectID == uuid.Nil {
			return errors.New("--id must be a project UUID")
		}
		var expectedValue *uint64
		if *expected != 0 {
			value := *expected
			expectedValue = &value
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		project, err := state.business.removeProject(context.Background(), projectID, expectedValue)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"removed": true, "project": projectCLIView(project)})
	default:
		return fmt.Errorf("unknown project command %q", arguments[0])
	}
}

func projectCLIViews(projects []registeredProject) []map[string]any {
	views := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		views = append(views, projectCLIView(project))
	}
	return views
}

// The CLI runs locally, so it may report the normalized local path. Remote
// RPC projections use peerProjectView and deliberately omit it.
func projectCLIView(project registeredProject) map[string]any {
	return map[string]any{
		"id": project.ID, "displayName": project.DisplayName, "path": project.LocalPath, "gitUrl": project.GitURL,
		"state": project.State, "revision": project.Revision,
		"policy": map[string]bool{
			"allowInteractiveTerminal": project.Policy.AllowInteractiveTerminal,
			"allowTaskExecution":       project.Policy.AllowTaskExecution,
			"allowAIWorkspaceTools":    project.Policy.AllowAIWorkspaceTools,
			"allowRemoteCreate":        true,
			"allowRecursiveDelete":     project.Policy.AllowRecursiveDelete,
		},
	}
}
