package bootstrap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/knotra-dev/engine/internal/contract"
)

// Connections contains public configuration and environment references, never credentials.
type Connections struct {
	Models    map[string]contract.ModelConnection `json:"models"`
	MCP       map[string]contract.MCPConnection   `json:"mcp,omitempty"`
	Secrets   map[string]contract.SecretSource    `json:"secrets"`
	Remove    []string                            `json:"remove,omitempty"`
	RemoveMCP []string                            `json:"removeMcp,omitempty"`
}

func readConnections(path string) (Connections, error) {
	var connections Connections
	file, err := os.Open(path)
	if err != nil {
		return connections, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return connections, err
	}
	if len(data) > 4<<20 {
		return connections, fmt.Errorf("connection configuration exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&connections); err != nil {
		return connections, fmt.Errorf("invalid connection configuration: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return connections, fmt.Errorf("invalid connection configuration")
	}
	for _, model := range connections.Models {
		switch model.Provider {
		case "openai", "anthropic", "openai_compatible", "ollama", "codex", "claude":
		default:
			return connections, fmt.Errorf("unsupported saved model provider")
		}
		for _, auth := range model.Auth {
			if auth.Value != nil {
				return connections, fmt.Errorf("model source credentials must use environment references")
			}
		}
	}
	for _, connection := range connections.MCP {
		for _, credentials := range []map[string]contract.Credential{connection.Headers, connection.Env} {
			for _, credential := range credentials {
				if credential.Value != nil {
					return connections, fmt.Errorf("MCP credentials must use environment references")
				}
			}
		}
	}
	return connections, nil
}

func checkedProfile(profile contract.Profile) ([]byte, error) {
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, diagnostics := contract.ParseProfile(data); contract.HasErrors(diagnostics) {
		return nil, fmt.Errorf("invalid connection profile: %v", diagnostics)
	}
	return data, nil
}

// ProfilePath overlays saved sources without rewriting a user's editable profile or run data.
func (s *Setup) ProfilePath() (string, error) {
	path := filepath.Join(s.Config.Dir, "profile.json")
	if s.connections == nil {
		return path, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	profile, diagnostics := contract.ParseProfile(data)
	if contract.HasErrors(diagnostics) {
		return "", fmt.Errorf("invalid saved profile: %v", diagnostics)
	}
	if profile.Spec.Models == nil {
		profile.Spec.Models = map[string]contract.ModelConnection{}
	}
	if profile.Spec.Secrets == nil {
		profile.Spec.Secrets = map[string]contract.SecretSource{}
	}
	if profile.Spec.MCP == nil {
		profile.Spec.MCP = map[string]contract.MCPConnection{}
	}
	for _, id := range s.connections.Remove {
		delete(profile.Spec.Models, id)
	}
	for id, model := range s.connections.Models {
		profile.Spec.Models[id] = model
	}
	for _, id := range s.connections.RemoveMCP {
		delete(profile.Spec.MCP, id)
	}
	for id, connection := range s.connections.MCP {
		profile.Spec.MCP[id] = connection
	}
	for id, secret := range s.connections.Secrets {
		profile.Spec.Secrets[id] = secret
	}
	data, err = checkedProfile(profile)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(s.Config.Dir, ".managed-profile-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	path = filepath.Join(s.Config.Dir, "managed-profile.json")
	return path, os.Rename(file.Name(), path)
}
