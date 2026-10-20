package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func configureCommands(root *cobra.Command) {
	root.SuggestionsMinimumDistance = 2
	root.Args = noArguments
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.Example = "  knotra init my-workflow\n  cd my-workflow\n  knotra validate\n  knotra run --profile local --watch\n  knotra runs\n  knotra status RUN_ID\n  knotra logs RUN_ID"
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting started:"},
		&cobra.Group{ID: "workflow", Title: "Work with workflows:"},
		&cobra.Group{ID: "inspect", Title: "Manage engine data:"},
		&cobra.Group{ID: "engine", Title: "Engine and configuration:"},
	)

	for _, cmd := range root.Commands() {
		switch cmd.Name() {
		case "init", "quickstart", "doctor":
			cmd.GroupID = "start"
		case "validate", "run":
			cmd.GroupID = "workflow"
		case "runs", "pipelines", "requests", "artifacts", "operations":
			cmd.GroupID = "inspect"
			cmd.SuggestionsMinimumDistance = 2
			for _, child := range cmd.Commands() {
				if child.Name() == "list" {
					cmd.Args = noArguments
					cmd.RunE = child.RunE
					cmd.Flags().AddFlagSet(child.Flags())
					child.Aliases = []string{"ls"}
				}
			}
			cmd.Long = cmd.Short + ".\nOmit the subcommand to list items. Use --help on a subcommand for its options."
		default:
			cmd.GroupID = "engine"
		}
		commandExamples(cmd)
		if cmd.Name() == "runs" {
			addRunShortcuts(root, cmd)
		}
	}
	root.SetHelpCommandGroupID("engine")
	root.SetCompletionCommandGroupID("engine")
}

func noArguments(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
			return fmt.Errorf("%w\nDid you mean: %s?", err, strings.Join(suggestions, ", "))
		}
		return err
	}
	return nil
}

func addRunShortcuts(root, runs *cobra.Command) {
	names := map[string]string{"show": "status", "watch": "logs", "cancel": "cancel"}
	for _, child := range runs.Commands() {
		name, ok := names[child.Name()]
		if !ok {
			continue
		}
		cmd := &cobra.Command{
			Use: name + " RUN_ID", Short: child.Short, Long: child.Long,
			Args: child.Args, RunE: child.RunE, GroupID: "workflow",
			Example: "  knotra " + name + " RUN_ID",
		}
		cmd.Flags().AddFlagSet(child.Flags())
		root.AddCommand(cmd)
	}
}

func commandExamples(cmd *cobra.Command) {
	switch cmd.Name() {
	case "quickstart":
		cmd.Example = "  knotra quickstart\n  knotra quickstart --provider openai --model MODEL_ID"
	case "doctor":
		cmd.Example = "  knotra doctor\n  knotra doctor --provider anthropic --model MODEL_ID"
	case "validate":
		cmd.Example = "  knotra validate\n  knotra validate pipeline.yaml --profile-file profile.yaml\n  knotra validate --remote --profile local"
	case "run":
		cmd.Example = "  knotra run --profile local --watch\n  knotra run pipeline.yaml -p local --input 'name=\"Ada\"' --wait\n  knotra run --definition PIPELINE_ID -p local --json"
	case "runs":
		cmd.Example = "  knotra runs\n  knotra runs show RUN_ID\n  knotra runs wait RUN_ID --timeout 5m\n  knotra runs instances RUN_ID --all"
	case "pipelines":
		cmd.Example = "  knotra pipelines\n  knotra pipelines publish pipeline.yaml\n  knotra pipelines show PIPELINE_ID --json"
	case "requests":
		cmd.Example = "  knotra requests --run RUN_ID\n  knotra requests respond REQUEST_ID --output approved=true"
	case "artifacts":
		cmd.Example = "  knotra artifacts --run RUN_ID\n  knotra artifacts download ARTIFACT_ID -o result.txt\n  knotra artifacts upload input.txt --media-type text/plain"
	case "operations":
		cmd.Example = "  knotra operations\n  knotra operations retry OPERATION_ID"
	case "serve":
		cmd.Example = "  knotra serve --profile profile.yaml\n  knotra serve --profile profile.yaml --listen 127.0.0.1:8787"
	case "profiles", "resources", "info":
		cmd.Example = "  knotra " + cmd.Name() + "\n  knotra " + cmd.Name() + " --json"
	}
}
