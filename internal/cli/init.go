package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/knotra-dev/engine/examples/starter"
)

func (s *commandState) initCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "init [DIRECTORY]",
		Short:   "Create an editable starter pipeline without connecting to an engine",
		Example: "  knotra init\n  knotra init my-pipeline",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			path, err := filepath.Abs(filepath.Join(dir, "pipeline.yaml"))
			if err != nil {
				return err
			}
			data, err := starter.Files.ReadFile("hello/pipeline.yaml")
			if err != nil {
				return err
			}
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return fmt.Errorf("create pipeline directory: %w", err)
			}
			if err = atomicFile(path, data, false); err != nil {
				if os.IsExist(err) {
					return fmt.Errorf("%q already exists; choose another directory or move it first", path)
				}
				return fmt.Errorf("create pipeline: %w", err)
			}
			if s.json {
				return s.printJSON(map[string]string{"path": path})
			}
			quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
			_, err = fmt.Fprintf(s.options.Out, "Created %q.\n\nValidate offline:\n  knotra validate %s\n\nRun with your local engine:\n  knotra run %s --profile local --wait\n", path, quoted, quoted)
			return err
		},
	}
}
