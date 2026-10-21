// Package cli is the command-line presentation layer for the Knotra engine API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/knotra-dev/engine/internal/client"
	"github.com/knotra-dev/engine/internal/contract"
)

type Options struct {
	Version  string
	In       io.Reader
	Out, Err io.Writer
	Getenv   func(string) string
}

type commandState struct {
	options            Options
	endpoint, stateDir string
	json               bool
	format             string
}

type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

func New(options Options) *cobra.Command {
	root, _ := newCommand(options)
	return root
}

func newCommand(options Options) (*cobra.Command, *commandState) {
	if options.In == nil {
		options.In = os.Stdin
	}
	if options.Out == nil {
		options.Out = os.Stdout
	}
	if options.Err == nil {
		options.Err = os.Stderr
	}
	if options.Getenv == nil {
		options.Getenv = os.Getenv
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	s := &commandState{options: options}
	root := &cobra.Command{
		Use:              "knotra",
		Short:            "Build and run AI workflows",
		Long:             "Knotra validates pipeline packages and runs them on an engine.\nStart with 'knotra init', or use 'knotra quickstart' to set up a local engine.",
		Version:          options.Version,
		SilenceUsage:     true,
		SilenceErrors:    true,
		TraverseChildren: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if s.format != "text" && s.format != "json" {
				return fmt.Errorf("--format must be text or json")
			}
			if s.json && cmd.Flags().Changed("format") && s.format == "text" {
				return fmt.Errorf("--json and --format text cannot be combined")
			}
			s.json = s.json || s.format == "json"
			return nil
		},
	}
	root.SetIn(options.In)
	root.SetOut(options.Out)
	root.SetErr(options.Err)
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	root.PersistentFlags().StringVarP(
		&s.endpoint,
		"endpoint",
		"e",
		s.env("KNOTRA_ENDPOINT", "http://127.0.0.1:8787"),
		"Engine base URL (KNOTRA_ENDPOINT)",
	)
	root.PersistentFlags().StringVar(
		&s.stateDir,
		"state-dir",
		s.env("KNOTRA_STATE_DIR", filepath.Join(dir, "knotra")),
		"Local durable command and event journal directory",
	)
	root.PersistentFlags().BoolVarP(&s.json, "json", "j", false, "Emit JSON (JSON Lines for events); same as --format json")
	root.PersistentFlags().StringVar(&s.format, "format", "text", "Output format: text or json")
	root.Flags().BoolP("version", "v", false, "Print the CLI version")
	root.SetVersionTemplate("knotra {{.Version}}\n")
	root.AddCommand(
		s.initCommand(),
		s.validateCommand(),
		s.serveCommand(),
		s.bootstrapCommand(false),
		s.bootstrapCommand(true),
		s.runCommand(),
		s.runsCommand(),
		s.definitionsCommand(),
		s.requestsCommand(),
		s.artifactsCommand(),
		s.operationsCommand(),
		s.catalogCommand("profiles"),
		s.catalogCommand("resources"),
		s.catalogCommand("info"),
		s.mcpCommand(),
	)
	configureCommands(root)
	return root, s
}

func Execute(ctx context.Context, args []string, options Options) error {
	root, state := newCommand(options)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err != nil {
		state.errorFormat(root, args)
		state.reportError(cmd, err)
	}
	return err
}

func (s *commandState) env(name, fallback string) string {
	if v := s.options.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func (s *commandState) client() *client.Client {
	return &client.Client{BaseURL: s.endpoint, StateDir: s.stateDir, Token: s.options.Getenv("KNOTRA_TOKEN")}
}

func (s *commandState) printJSON(v any) error {
	encoder := json.NewEncoder(s.options.Out)
	encoder.SetEscapeHTML(false)
	if !s.json {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(v)
}

func (s *commandState) table(headers []string, rows [][]string) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(s.options.Out, "No results.")
		return err
	}

	writer := tabwriter.NewWriter(s.options.Out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, strings.Join(headers, "\t")); err != nil {
		return err
	}

	for _, row := range rows {
		for i := range row {
			row[i] = safeText(row[i])
		}

		if _, err := fmt.Fprintln(writer, strings.Join(row, "\t")); err != nil {
			return err
		}
	}

	return writer.Flush()
}

func safeText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
}

func (s *commandState) diagnostics(d []contract.Diagnostic) error {
	for _, item := range d {
		position := item.File
		if item.Line > 0 {
			position += fmt.Sprintf(":%d:%d", item.Line, item.Column)
		}
		if position == "" {
			position = item.Path
		}
		if _, err := fmt.Fprintf(
			s.options.Out,
			"%s %s %s: %s\n",
			strings.ToUpper(item.Severity),
			safeText(item.Code),
			safeText(position),
			safeText(item.Message),
		); err != nil {
			return err
		}
	}

	return nil
}

func (s *commandState) catalogCommand(kind string) *cobra.Command {
	return &cobra.Command{Use: kind, Short: "Show engine " + kind, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var response map[string]any
		if err := s.client().Get(cmd.Context(), "/"+kind, &response); err != nil {
			return err
		}
		if s.json {
			return s.printJSON(response)
		}
		if kind == "info" {
			return s.printInfo(response)
		}

		items, _ := response["items"].([]any)
		rows := [][]string{}

		for _, item := range items {
			record, _ := item.(map[string]any)
			rows = append(
				rows,
				[]string{
					stringField(record, "id"),
					stringField(record, "kind"),
					stringField(record, "title"),
					stringField(record, "status"),
					stringField(record, "revision"),
				},
			)
		}

		return s.table([]string{"ID", "KIND", "TITLE", "STATUS", "REVISION"}, rows)
	}}
}

func stringField(m map[string]any, key string) string {
	if value, ok := m[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}
