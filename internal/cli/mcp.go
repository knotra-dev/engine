package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/knotra-dev/engine/internal/adapters"
	"github.com/knotra-dev/engine/internal/contract"
)

type mcpDiscoveryInput struct {
	URL     string                           `json:"url"`
	Headers map[string]contract.Credential   `json:"headers,omitempty"`
	Secrets map[string]contract.SecretSource `json:"secrets,omitempty"`
}

func (s *commandState) mcpCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Connect assistants to Knotra and inspect MCP servers"}
	cmd.AddCommand(s.desktopMCPCommand())
	cmd.AddCommand(&cobra.Command{
		Use: "discover", Short: "List HTTP MCP tools without calling them", Args: cobra.NoArgs,
		Long: "List an HTTP MCP server's tools without calling tools. Read JSON {url, headers, secrets} from stdin. Headers use secretRef credentials and secrets use environment references; no credential values are accepted in JSON. HTTPS is required except on loopback.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			profile, connection, err := s.mcpDiscoveryProfile()
			if err != nil {
				return err
			}

			runner := &adapters.Runner{LookupEnv: func(name string) (string, bool) {
				value := s.options.Getenv(name)
				return value, value != ""
			}}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			catalog, discoveryErr := runner.DiscoverMCPHTTP(ctx, profile, connection)
			cleanupErr := runner.Close()
			if cleanupErr != nil {
				return fmt.Errorf("MCP temporary session cleanup failed; check the server before trying again")
			}
			if discoveryErr != nil {
				if ctx.Err() != nil {
					return errors.Join(fmt.Errorf("MCP discovery stopped before the server was ready"), ctx.Err())
				}
				// A remote error may contain credentials; never send its text to the UI.
				return fmt.Errorf("MCP discovery failed; check the URL, access token and server availability")
			}

			type tool struct {
				Name string `json:"name"`
				contract.ToolSnapshot
			}
			tools := make([]tool, 0, len(catalog))
			for name, snapshot := range catalog {
				if bytes.Equal(snapshot.OutputSchema, []byte("null")) {
					snapshot.OutputSchema = nil
				}
				tools = append(tools, tool{Name: name, ToolSnapshot: snapshot})
			}
			slices.SortFunc(tools, func(a, b tool) int { return strings.Compare(a.Name, b.Name) })
			return s.printJSON(struct {
				Tools []tool `json:"tools"`
			}{Tools: tools})
		},
	})
	return cmd
}

func (s *commandState) mcpDiscoveryProfile() (contract.Profile, contract.MCPConnection, error) {
	var input mcpDiscoveryInput
	var empty contract.Profile
	var connection contract.MCPConnection
	data, err := io.ReadAll(io.LimitReader(s.options.In, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || !json.Valid(data) {
		return empty, connection, fmt.Errorf("MCP discovery expects a JSON object up to 64 KiB on stdin")
	}
	if _, err := contract.ParseDocument(data); err != nil {
		return empty, connection, fmt.Errorf("MCP discovery configuration is invalid")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return empty, connection, fmt.Errorf("MCP discovery expects only url, headers and secrets")
	}

	u, err := url.Parse(input.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(input.URL, "#") {
		return empty, connection, fmt.Errorf("MCP URL must be an absolute HTTPS URL without credentials, query or fragment")
	}

	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return empty, connection, fmt.Errorf("MCP URL requires HTTPS; HTTP is allowed only on loopback")
	}

	for _, credential := range input.Headers {
		if credential.Value != nil || credential.SecretRef == "" {
			return empty, connection, fmt.Errorf("MCP header credentials must use environment references")
		}
	}
	connection = contract.MCPConnection{Transport: "streamable_http", URL: input.URL, Headers: input.Headers, AllowedTools: []string{}}
	profile := contract.Profile{
		APIVersion: "knotra/v1",
		Kind:       "EngineProfile",
		Metadata:   contract.Metadata{Name: "discovery"},
		Spec: contract.ProfileSpec{
			Secrets: input.Secrets, MCP: map[string]contract.MCPConnection{"discovery": connection},
			Limits: contract.Limits{Timeout: "20s", MaxConcurrentNodes: 1, MaxNodeInstances: 1, MaxModelCalls: 1, MaxToolCalls: 1},
		},
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return empty, connection, fmt.Errorf("MCP discovery configuration is invalid")
	}
	if _, diagnostics := contract.ParseProfile(encoded); contract.HasErrors(diagnostics) {
		return empty, connection, fmt.Errorf("MCP discovery headers or environment references are invalid")
	}

	for _, credential := range input.Headers {
		value := s.options.Getenv(input.Secrets[credential.SecretRef].Env)
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return empty, connection, fmt.Errorf("MCP header credential is unavailable or invalid")
		}
	}
	return profile, connection, nil
}
