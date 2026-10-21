package cli

import "encoding/json"

type desktopControlTool struct {
	name, title, description string
	schema                   json.RawMessage
	readOnly                 bool
}

func desktopObject(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func desktopString(description string) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "description": description}
}

func desktopEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func desktopMutation(properties map[string]any, required ...string) map[string]any {
	properties["requestId"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{8,128}$", "description": "A unique ID for this action, for example a UUID. Replay protection lasts only for the current app session. After app restart, read and reconcile state before retrying an uncertain action."}
	return desktopObject(properties, append(required, "requestId")...)
}

func desktopControlTools() []desktopControlTool {
	id := desktopString("The item's ID from its list or get result.")
	revision := desktopString("The exact revision returned by pipeline_get. Read the pipeline again after a conflict.")
	boolean := map[string]any{"type": "boolean"}
	file := desktopObject(map[string]any{"path": desktopString("A relative package file path."), "content": map[string]any{"type": "string", "description": "File bytes encoded as base64, including text files."}}, "path", "content")
	files := map[string]any{"type": "array", "items": file}
	positions := map[string]any{"type": "object", "additionalProperties": desktopObject(map[string]any{"x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}}, "x", "y")}
	provider := desktopEnum("openai", "anthropic", "openai-compatible", "ollama", "codex", "claude")
	sourceRevision := map[string]any{"type": "integer", "minimum": 0, "maximum": 9007199254740991, "description": "The connection.revision from the latest source list. Required when changing an existing source."}
	sourceSave := func(properties map[string]any, required ...string) map[string]any {
		properties["expectedRevision"] = sourceRevision
		schema := desktopMutation(properties, required...)
		schema["dependentRequired"] = map[string]any{"id": []string{"expectedRevision"}}
		return schema
	}
	tools := []desktopControlTool{}
	add := func(name, title, description string, schema any, readOnly bool) {
		data, _ := json.Marshal(schema)
		tools = append(tools, desktopControlTool{name: name, title: title, description: description, schema: data, readOnly: readOnly})
	}
	add("app_status", "App status", "Read the current app section, active pipeline and engine state. No credentials are returned.", desktopObject(map[string]any{}), true)
	add("app_navigate", "Open an app section", "Show a pipeline, run or Settings section in the live app.", desktopMutation(map[string]any{"section": desktopEnum("pipelines", "runs", "inbox", "artifacts", "connections", "settings"), "workspaceId": id, "runId": id}, "section"), false)
	add("pipeline_list", "List pipelines", "List pipeline IDs, names, revisions and unsaved-edit state.", desktopObject(map[string]any{}), true)
	add("pipeline_get", "Read a pipeline", "Read the complete editable pipeline package, positions and current revision. Read before changing or deleting a pipeline.", desktopObject(map[string]any{"id": id}, "id"), true)
	add("pipeline_create", "Create a pipeline", "Create a pipeline in the live app. Read knotra://guide and knotra://schema/v1 first. The default entrypoint is pipeline.yaml; activate defaults to true.", desktopMutation(map[string]any{"source": desktopString("The full Knotra pipeline YAML or JSON source."), "entrypoint": desktopString("Pipeline entrypoint path, default pipeline.yaml."), "files": files, "activate": boolean}, "source"), false)
	add("pipeline_update", "Edit a pipeline", "Edit YAML, package files or graph positions in the live app. Omitted fields are preserved. Existing unsaved edits are protected unless allowDirty is explicitly true. activate defaults to true.", desktopMutation(map[string]any{"id": id, "expectedRevision": revision, "source": desktopString("Replacement pipeline YAML or JSON source."), "entrypoint": desktopString("Replacement entrypoint path."), "files": files, "positions": positions, "allowDirty": boolean, "activate": boolean}, "id", "expectedRevision"), false)
	add("pipeline_delete", "Delete a pipeline", "Delete a pipeline after reading its current revision. Unsaved edits require allowDirty:true.", desktopMutation(map[string]any{"id": id, "expectedRevision": revision, "allowDirty": boolean}, "id", "expectedRevision"), false)
	add("pipeline_validate", "Check a pipeline", "Run the app's local pipeline validation and return diagnostics. This does not execute a workflow.", desktopObject(map[string]any{"id": id}, "id"), true)
	add("pipeline_publish", "Publish a pipeline to the engine", "Publish the exact current package as an immutable engine definition. Use a stable operationId to retry a publish safely; this does not start a run.", desktopMutation(map[string]any{"id": id, "expectedRevision": revision, "operationId": desktopString("A unique durable engine operation ID; reuse only for the same publish.")}, "id", "expectedRevision", "operationId"), false)
	add("model_source_list", "List model sources", "List saved providers and their discovered model IDs. Credentials are never returned.", desktopObject(map[string]any{}), true)
	add("model_source_catalog", "Discover provider models", "Fetch a provider's model catalog before saving a source. apiKey is write-only; Codex and Claude use their existing CLI account login.", desktopObject(map[string]any{"provider": provider, "apiKey": desktopString("A write-only provider API key."), "baseUrl": desktopString("The provider's API base URL, when needed.")}, "provider"), true)
	add("model_source_save", "Save a model source", "Add or edit a named model source. For an edit, read the source list and pass its current expectedRevision. Omit apiKey to retain its existing credential. Changes apply automatically at a safe idle point and appear in Settings.", sourceSave(map[string]any{"id": id, "title": desktopString("A name that explains this source."), "provider": provider, "apiKey": desktopString("A write-only provider API key."), "baseUrl": desktopString("The provider's API base URL."), "model": desktopString("An optional default model ID from the provider catalog.")}, "provider"), false)
	add("model_source_refresh", "Refresh source models", "Refresh a saved source's model catalog using its saved credential and latest revision.", desktopMutation(map[string]any{"id": id, "expectedRevision": sourceRevision}, "id", "expectedRevision"), false)
	add("model_source_remove", "Remove a model source", "Remove a saved model source at its current revision. Existing unfinished runs retain their admitted source until safe application.", desktopMutation(map[string]any{"id": id, "expectedRevision": sourceRevision}, "id", "expectedRevision"), false)
	add("model_source_sign_in", "Sign in to a CLI provider", "Start the existing Codex or Claude account sign-in flow. The user must complete the login. Check sign-in status for the link or code.", desktopMutation(map[string]any{"provider": desktopEnum("codex", "claude")}, "provider"), false)
	add("model_source_sign_in_status", "Read provider sign-in status", "Read the provider sign-in progress, user login link or code. This does not expose saved API keys.", desktopObject(map[string]any{}), true)
	add("model_source_sign_in_cancel", "Cancel provider sign-in", "Cancel the active provider sign-in flow.", desktopMutation(map[string]any{}), false)
	add("mcp_server_list", "List external MCP servers", "List saved external MCP servers, discovered tools and explicit tool allowlists. Tokens are never returned.", desktopObject(map[string]any{}), true)
	add("mcp_server_discover", "Discover external MCP tools", "List tools from an HTTP MCP URL without executing tools. The optional bearer token is write-only. A saved token is retained only for the exact saved URL.", desktopObject(map[string]any{"id": id, "url": desktopString("An HTTPS MCP endpoint, or HTTP on loopback."), "token": desktopString("A write-only bearer token, without the Bearer prefix."), "clearToken": boolean}, "url"), true)
	add("mcp_server_save", "Save an external MCP server", "Add or edit an external HTTP MCP server and select its tools explicitly. Editing requires the source list's current expectedRevision. allowedTools:[] permits no tools. Updates apply automatically when workflows are idle.", sourceSave(map[string]any{"id": id, "title": desktopString("A readable server name."), "url": desktopString("An HTTPS MCP endpoint, or HTTP on loopback."), "token": desktopString("A write-only bearer token, without the Bearer prefix."), "clearToken": boolean, "allowedTools": map[string]any{"type": "array", "maxItems": 1024, "uniqueItems": true, "items": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_.-]{1,128}$"}}}, "url", "allowedTools"), false)
	add("mcp_server_refresh", "Refresh external MCP tools", "Refresh a saved server's catalog at its current revision without granting newly discovered tools or dropping existing grants.", desktopMutation(map[string]any{"id": id, "expectedRevision": sourceRevision}, "id", "expectedRevision"), false)
	add("mcp_server_remove", "Remove an external MCP server", "Remove a saved external MCP server at its current revision. Changes wait for unfinished workflows before replacing the engine configuration.", desktopMutation(map[string]any{"id": id, "expectedRevision": sourceRevision}, "id", "expectedRevision"), false)
	add("engine_status", "Read engine status", "Read the app's current engine connection, local process state and pending source updates.", desktopObject(map[string]any{}), true)
	add("engine_start", "Start the local engine", "Start or reconnect to the saved local engine and update the live app session.", desktopMutation(map[string]any{}), false)
	add("engine_stop", "Stop the local engine", "Explicitly stop the app-owned local engine. Check for unfinished workflows first.", desktopMutation(map[string]any{}), false)
	add("engine_connect", "Connect the app to an engine", "Connect the live app to an existing engine URL. The optional access token is session-only and write-only. Remote engines require HTTPS and authentication.", desktopMutation(map[string]any{"url": desktopString("The engine base URL."), "token": desktopString("A write-only session access token.")}, "url"), false)
	add("engine_disconnect", "Disconnect the app from its engine", "Disconnect the live app's engine session without stopping an external engine.", desktopMutation(map[string]any{}), false)
	add("artifact_download", "Read a generated file", "Download an engine artifact through the app's integrity checks. Returns artifact metadata and base64 content. The bridge response is limited to 8 MiB; larger files must be opened in the app.", desktopObject(map[string]any{"artifactId": id}, "artifactId"), true)
	add("engine_call", "Run or inspect engine work", "Use the app's closed engine command vocabulary. Read profiles/resources, validate/publish packages, start/observe/cancel/resume runs, respond to human requests, resolve attempts, or upload/read artifact metadata. Writes require durable operationId values. Pagination uses cursor/nextCursor. No arbitrary URLs or HTTP methods are accepted.", desktopEngineCallSchema(file, files), false)
	return tools
}

func desktopEngineCallSchema(file, files any) map[string]any {
	id := desktopString("An ID returned by the engine.")
	operationID := map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$", "description": "A unique durable operation ID, for example a UUID. Reuse only when retrying the same engine command."}
	object := map[string]any{"type": "object", "additionalProperties": true}
	pkg := desktopObject(map[string]any{"entrypoint": desktopString("Relative pipeline entrypoint."), "source": desktopString("Pipeline YAML or JSON source."), "files": files}, "entrypoint", "source", "files")
	variants := []any{}
	add := func(op string, properties map[string]any, required ...string) {
		properties["op"] = map[string]any{"const": op}
		variants = append(variants, desktopObject(properties, append(required, "op")...))
	}
	for _, op := range []string{"info", "definitions", "resources", "profiles"} {
		add(op, map[string]any{})
	}
	for _, op := range []string{"runs", "requests", "artifacts"} {
		add(op, map[string]any{"cursor": id})
	}
	add("run", map[string]any{"runId": id}, "runId")
	add("instances", map[string]any{"runId": id, "cursor": id}, "runId")
	add("history", map[string]any{"runId": id, "instanceId": id, "cursor": id}, "runId")
	add("definition", map[string]any{"definitionId": id}, "definitionId")
	add("validate", map[string]any{"package": pkg, "profile": id, "inputs": object, "artifacts": object}, "package", "profile", "inputs", "artifacts")
	add("publish", map[string]any{"package": pkg, "operationId": operationID}, "package", "operationId")
	add("start", map[string]any{"definitionId": id, "profile": id, "inputs": object, "artifacts": object, "operationId": operationID}, "definitionId", "profile", "inputs", "artifacts", "operationId")
	for _, op := range []string{"cancel", "resume"} {
		add(op, map[string]any{"runId": id, "operationId": operationID}, "runId", "operationId")
	}
	add("respond", map[string]any{"requestId": id, "outputs": object, "operationId": operationID}, "requestId", "outputs", "operationId")
	add("resolve", map[string]any{"runId": id, "instanceId": id, "resolution": object, "operationId": operationID}, "runId", "instanceId", "resolution", "operationId")
	add("upload", map[string]any{"file": file, "mediaType": id, "operationId": operationID}, "file", "mediaType", "operationId")
	add("artifact", map[string]any{"artifactId": id}, "artifactId")
	return map[string]any{"type": "object", "oneOf": variants}
}

const desktopControlGuide = `# Control Knotra

The desktop app must be running with its workspace loaded. Tool calls update the same pipelines, connections and engine session visible in the app. Saved credentials are write-only. Do not ask tools to return API keys or tokens.

## Edit a pipeline

1. Call pipeline_list, then pipeline_get with its ID. Keep the returned revision and inspect dirty before editing.
2. Read knotra://schema/v1 for the exact Pipeline contract. Source YAML drives the visual graph; editing source edits its nodes and connections. Keep package files unless intentionally replacing them. File content is base64, including text files.
3. Call pipeline_update with id, expectedRevision, a unique requestId and only the fields being changed. Omitted fields are preserved. Existing unsaved edits require allowDirty:true; use that only after reading and intentionally retaining or replacing those edits. Updates appear live in the app and are saved as local drafts.
4. On a conflict or busy edit, read the pipeline again. Never overwrite an unseen newer revision. pipeline_validate returns local diagnostics; pipeline_publish creates an immutable engine definition and requires its own durable operationId.

For a new pipeline use pipeline_create. Example source:

apiVersion: knotra/v1
kind: Pipeline
metadata: {name: review, title: Review a result}
spec:
  nodes:
    approve:
      type: human
      human: {prompt: {text: Approve the result}}
      outputs: {approved: {schema: {type: boolean}}}
  outputs:
    approved: {schema: {type: boolean}, bind: {from: nodes.approve.outputs.approved}}

## Connect models and tools

model_source_list returns source IDs and actual model IDs. Discover catalogs before saving new API providers; Codex and Claude sign-in require the user to finish account login. Assign model connections and model IDs to each pipeline node's declared model alias. Use engine_call op:resources and op:profiles for engine resource/sandbox profile IDs.

Before editing, refreshing or removing an existing model source or MCP server, list its connections and pass connection.revision as expectedRevision. Creating a new source omits id and expectedRevision. A stale revision is rejected; list again instead of overwriting newer settings.

mcp_server_discover lists an external HTTP server's tools without calling them. mcp_server_save stores an explicit allowedTools list. New tools are never automatically granted. Use those server connection IDs and tool names in the pipeline's spec.mcp declarations and Tool/Agent nodes. Source changes are queued until unfinished runs, including human waits, finish.

## Run and observe

engine_call is a closed command union, not an HTTP proxy. Use op:profiles/resources first, publish a package or pipeline_publish, then op:start with definitionId, profile, inputs:{}, artifacts:{}, and a unique operationId. Observe op:run, op:instances and op:history (cursor pagination); list op:requests for human work. Respond using op:respond with requestId, outputs and operationId. Cancel/resume use runId and operationId; resolve uses runId, instanceId, resolution and operationId. Engine mutations keep durable receipts.

Use artifact_download with an artifactId to inspect generated files. It returns integrity-checked metadata and base64 bytes, within the bridge's 8 MiB response limit. Larger files remain available in the app.

Every other mutating tool requires requestId. These replay receipts exist only in the current app session; they are not durable across app restarts. Within that session, reuse the same ID and identical arguments after an uncertain response. After the app restarts, first list/read and reconcile the latest pipeline and source state before retrying an uncertain action; an old requestId alone does not prevent another create. A definitive busy rejection requires a new requestId after reading the latest state. Reusing an ID for different arguments is rejected. The bridge makes no automatic mutation retries. engine_call writes use their separate durable operationId receipts, which survive restarts.
`
