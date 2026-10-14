package mcpremote

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// toolsJSON is the shared tool catalog (SPEC §14.1, normative): the Go
// server embeds it and the Cloudflare edition generates
// src/generated/mcp.ts from it, so both list the same tools byte for byte.
//
//go:embed tools.json
var toolsJSON []byte

// ToolsJSON returns the embedded tool catalog (tools.json).
func ToolsJSON() []byte { return bytes.Clone(toolsJSON) }

// catalog is the parsed tools.json.
type catalog struct {
	instructions string
	// tools is the "tools" array, compacted, verbatim: tools/list returns it.
	tools   json.RawMessage
	names   []string
	schemas map[string]*jsonschema.Resolved
}

// loadCatalog parses and resolves the embedded catalog once.
var loadCatalog = sync.OnceValues(func() (*catalog, error) { return parseCatalog(toolsJSON) })

// parseCatalog parses a catalog and resolves the input schema of every tool.
func parseCatalog(b []byte) (*catalog, error) {
	var doc struct {
		Instructions string          `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("mcpremote: tools.json: %w", err)
	}
	var tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(doc.Tools, &tools); err != nil {
		return nil, fmt.Errorf("mcpremote: tools.json tools: %w", err)
	}
	if doc.Instructions == "" || len(tools) == 0 {
		return nil, errors.New("mcpremote: tools.json needs instructions and tools")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, doc.Tools); err != nil {
		return nil, fmt.Errorf("mcpremote: tools.json tools: %w", err)
	}
	c := &catalog{instructions: doc.Instructions, tools: compact.Bytes(), schemas: make(map[string]*jsonschema.Resolved, len(tools))}
	for _, t := range tools {
		if t.Name == "" || c.schemas[t.Name] != nil {
			return nil, fmt.Errorf("mcpremote: tools.json: missing or duplicate tool name %q", t.Name)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(t.InputSchema, &s); err != nil {
			return nil, fmt.Errorf("mcpremote: tools.json: input schema of %s: %w", t.Name, err)
		}
		rs, err := s.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("mcpremote: tools.json: input schema of %s: %w", t.Name, err)
		}
		c.names = append(c.names, t.Name)
		c.schemas[t.Name] = rs
	}
	return c, nil
}
