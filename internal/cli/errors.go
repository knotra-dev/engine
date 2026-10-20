package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/knotra-dev/engine/internal/client"
	"github.com/knotra-dev/engine/internal/contract"
)

func (s *commandState) errorFormat(root *cobra.Command, args []string) {
	flags := pflag.NewFlagSet("error output", pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.AddFlagSet(root.PersistentFlags())
	var addFlags func(*cobra.Command)
	addFlags = func(cmd *cobra.Command) {
		flags.AddFlagSet(cmd.Flags())
		for _, child := range cmd.Commands() {
			addFlags(child)
		}
	}
	addFlags(root)
	jsonOutput, format := false, "text"
	// Read output flags without reapplying values or rejecting unrelated values.
	_ = flags.ParseAll(args, func(flag *pflag.Flag, value string) error {
		switch flag.Name {
		case "json":
			if parsed, err := strconv.ParseBool(value); err == nil {
				jsonOutput = parsed
			}
		case "format":
			format = value
		}
		return nil
	})
	s.json, s.format = jsonOutput, format
}

func (s *commandState) reportError(cmd *cobra.Command, err error) {
	path := "knotra"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	code, hint := "COMMAND_ERROR", "Run '"+path+" --help' for usage and examples."
	var diagnostics []contract.Diagnostic
	var httpErr *client.HTTPError
	var transportErr *url.Error
	switch {
	case errors.Is(err, context.Canceled):
		code, hint = "INTERRUPTED", "Observation stopped; any submitted run continues on the engine."
	case errors.Is(err, context.DeadlineExceeded):
		code, hint = "TIMEOUT", "Observation timed out; check the run with 'knotra status RUN_ID'."
	case errors.As(err, &httpErr):
		code = httpErr.Body.Code
		diagnostics = httpErr.Body.Diagnostics
		if httpErr.Status == 401 || httpErr.Status == 403 {
			hint = "Check KNOTRA_TOKEN and your access to this engine."
		}
	case errors.As(err, &transportErr):
		code, hint = "CONNECTION_ERROR", "Check --endpoint or KNOTRA_ENDPOINT. Start a local engine with 'knotra quickstart'."
	}
	if s.json || s.format == "json" {
		_ = json.NewEncoder(s.options.Err).Encode(map[string]any{
			"error":    map[string]any{"code": code, "message": err.Error(), "hint": hint, "diagnostics": diagnostics},
			"exitCode": ExitCode(err),
		})
		return
	}
	_, _ = fmt.Fprintln(s.options.Err, "Error:", safeText(err.Error()))
	for _, item := range diagnostics {
		_, _ = fmt.Fprintf(s.options.Err, "%s %s: %s\n", safeText(item.Code), safeText(item.Path), safeText(item.Message))
	}
	_, _ = fmt.Fprintln(s.options.Err, safeText(hint))
}
