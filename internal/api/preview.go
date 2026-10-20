package api

import (
	"encoding/json"
	"net/http"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func javascriptPreview(r *http.Request) bool {
	return r.URL.Query().Get("preview") == "javascript"
}

func previewJSONPorts(values map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	preview := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		decoded, err := contract.DecodeJSON(value)
		if err == nil && store.ClientPreviewNumbers(decoded) {
			preview[name] = value
		}
	}
	return preview, len(preview) != len(values)
}

func previewRun(run protocol.Run) protocol.Run {
	var inputsOmitted, outputsOmitted bool
	run.Inputs, inputsOmitted = previewJSONPorts(run.Inputs)
	run.Outputs, outputsOmitted = previewJSONPorts(run.Outputs)
	run.DataTruncated = run.DataTruncated || inputsOmitted || outputsOmitted
	return run
}

func previewRequest(request protocol.HumanRequest) protocol.HumanRequest {
	if inputs, ok := request.Inputs.(map[string]any); ok {
		if values, ok := inputs["values"].(map[string]any); ok {
			preview := make(map[string]any, len(values))
			for name, value := range values {
				if store.ClientPreviewNumbers(value) {
					preview[name] = value
				}
			}
			request.Inputs = map[string]any{"values": preview, "artifacts": inputs["artifacts"]}
			request.DataTruncated = request.DataTruncated || len(preview) != len(values)
		}
	}
	decoded, err := contract.DecodeJSON(request.ResponseSchema)
	if err != nil || !store.ClientPreviewNumbers(decoded) {
		// Keep the authored schema exact as text; a partial schema cannot authorize a response.
		request.ResponseSchemaJSON = string(request.ResponseSchema)
		request.ResponseSchema = json.RawMessage("null")
		request.DataTruncated = true
	}
	return request
}
