package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

func (s *commandState) fields(rows [][]string) error {
	writer := tabwriter.NewWriter(s.options.Out, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		if _, err := fmt.Fprintf(writer, "%s:\t%s\n", safeText(row[0]), safeText(row[1])); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func (s *commandState) printDefinition(definition protocol.Definition) error {
	return s.fields([][]string{
		{"Pipeline", definition.ID},
		{"Name", definition.Name},
		{"Title", definition.Title},
		{"Digest", definition.PackageDigest},
		{"Created", definition.CreatedAt.Format(time.RFC3339)},
	})
}

func (s *commandState) printArtifact(artifact contract.Artifact) error {
	return s.fields([][]string{
		{"Artifact", artifact.ID},
		{"Name", artifact.Name},
		{"Media type", artifact.MediaType},
		{"Bytes", fmt.Sprint(artifact.Size)},
		{"SHA-256", artifact.SHA256},
	})
}

func (s *commandState) printInfo(info map[string]any) error {
	return s.fields([][]string{
		{"Endpoint", s.endpoint},
		{"Engine", stringField(info, "engineId")},
		{"Version", stringField(info, "version")},
		{"Protocol", stringField(info, "protocol")},
		{"Principal", stringField(info, "principalId")},
		{"Host", stringField(info, "hostId")},
		{"Backend", stringField(info, "backend")},
		{"Execution timeout", stringField(info, "executionTimeout")},
		{"Capabilities", stringField(info, "capabilities")},
	})
}

func (s *commandState) printMutation(result map[string]any) error {
	if s.json {
		return s.printJSON(result)
	}

	var rows [][]string
	switch {
	case result["definition"] != nil:
		definition, _ := result["definition"].(map[string]any)
		rows = [][]string{
			{"Pipeline", stringField(definition, "id")},
			{"Name", stringField(definition, "name")},
			{"Digest", stringField(definition, "packageDigest")},
		}
	case result["artifact"] != nil:
		artifact, _ := result["artifact"].(map[string]any)
		rows = [][]string{
			{"Artifact", stringField(artifact, "id")},
			{"Name", stringField(artifact, "name")},
			{"Media type", stringField(artifact, "mediaType")},
			{"Bytes", stringField(artifact, "size")},
		}
	case result["run"] != nil:
		run, _ := result["run"].(map[string]any)
		rows = [][]string{{"Run", stringField(run, "id")}, {"Status", stringField(run, "status")}, {"Profile", stringField(run, "profile")}}
	default:
		rows = [][]string{{"Accepted", stringField(result, "accepted")}}
		if id := stringField(result, "runId"); id != "" {
			rows = append(rows, []string{"Run", id})
		}
		if id := stringField(result, "requestId"); id != "" {
			rows = append(rows, []string{"Request", id})
		}
	}
	if id := stringField(result, "operationId"); id != "" {
		rows = append(rows, []string{"Operation", id})
	}
	return s.fields(rows)
}
