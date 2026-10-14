package mcpremote

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestCatalogSchemaRules checks the catalog rules of SPEC §14.1 that keep
// the catalog usable by every MCP client (Claude rejects tools that break
// them).
func TestCatalogSchemaRules(t *testing.T) {
	var doc struct {
		Instructions string           `json:"instructions"`
		Tools        []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(toolsJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(doc.Instructions) == "" {
		t.Fatal("no instructions")
	}
	var names []string
	propName := regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	for _, tool := range doc.Tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
		if name == "" || len(name) > 64 {
			t.Errorf("tool name %q: 1-64 characters", name)
		}
		if title, _ := tool["title"].(string); title == "" {
			t.Errorf("%s: no title", name)
		}
		if desc, _ := tool["description"].(string); desc == "" {
			t.Errorf("%s: no description", name)
		}
		ann, _ := tool["annotations"].(map[string]any)
		if title, _ := ann["title"].(string); title == "" {
			t.Errorf("%s: no annotations.title", name)
		}
		for _, k := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := ann[k].(bool); !ok {
				t.Errorf("%s: annotations.%s is not a boolean", name, k)
			}
		}
		if _, ok := tool["outputSchema"]; ok {
			t.Errorf("%s: outputSchema (results are text only)", name)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Errorf("%s: inputSchema.type = %v", name, schema["type"])
		}
		for _, k := range []string{"anyOf", "oneOf", "allOf"} {
			if _, ok := schema[k]; ok {
				t.Errorf("%s: root %s", name, k)
			}
		}
		walkSchema(schema, func(s map[string]any) {
			if _, ok := s["$schema"]; ok {
				t.Errorf("%s: $schema key", name)
			}
			props, _ := s["properties"].(map[string]any)
			for p := range props {
				if !propName.MatchString(p) {
					t.Errorf("%s: property name %q", name, p)
				}
			}
		})
	}
	want := []string{"backyard_docs", "backyard_whoami", "backyard_list_sites", "backyard_site_files", "backyard_read_site_file", "backyard_publish"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	// Only backyard_publish changes anything.
	for _, tool := range doc.Tools {
		ann := tool["annotations"].(map[string]any)
		publish := tool["name"] == "backyard_publish"
		if ann["readOnlyHint"] == publish || ann["destructiveHint"] != publish || ann["openWorldHint"] != false {
			t.Errorf("%s: annotations %v", tool["name"], ann)
		}
	}
}

// walkSchema calls fn for s and every subschema below it.
func walkSchema(s map[string]any, fn func(map[string]any)) {
	if s == nil {
		return
	}
	fn(s)
	if props, ok := s["properties"].(map[string]any); ok {
		for _, p := range props {
			sub, _ := p.(map[string]any)
			walkSchema(sub, fn)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		walkSchema(items, fn)
	}
}

func TestCatalogResolves(t *testing.T) {
	c, err := parseCatalog(toolsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.schemas) != 6 || len(c.names) != 6 {
		t.Fatalf("catalog = %v", c.names)
	}
	// The resolved schemas enforce the keywords the catalog uses.
	check := func(tool, args string, valid bool) {
		t.Helper()
		var inst any
		if err := json.Unmarshal([]byte(args), &inst); err != nil {
			t.Fatal(err)
		}
		if err := c.schemas[tool].Validate(inst); (err == nil) != valid {
			t.Errorf("%s %s: valid = %v (%v)", tool, args, err == nil, err)
		}
	}
	check("backyard_whoami", `{}`, true)
	check("backyard_whoami", `{"x":1}`, false)
	check("backyard_docs", `{"topic":"api"}`, true)
	check("backyard_docs", `{"topic":"x"}`, false)
	check("backyard_list_sites", `{"query":"`+strings.Repeat("é", 200)+`"}`, true) // code points, not bytes
	check("backyard_list_sites", `{"query":"`+strings.Repeat("a", 201)+`"}`, false)
	check("backyard_site_files", `{}`, false)
	check("backyard_site_files", `{"site":"lunch-poll"}`, true)
	check("backyard_site_files", `{"site":"-x"}`, false)
	check("backyard_read_site_file", `{"site":"a","path":""}`, false)
	check("backyard_publish", `{"site":"a","files":[]}`, true)
	check("backyard_publish", `{"site":"a","files":[{"path":"a","content":"x","encoding":"base64","content_type":"image/png"}],"delete":["b"],"replace":false,"overwrite":true}`, true)
	check("backyard_publish", `{"site":"a","files":[{"path":"a"}]}`, false)
	check("backyard_publish", `{"site":"a","files":[{"path":"a","content":"x","mode":"0644"}]}`, false)
	check("backyard_publish", `{"site":"a","files":[],"replace":"yes"}`, false)
	check("backyard_publish", `{"site":"a","files":[],"delete":[""]}`, false)

	// Bad catalogs are refused.
	for _, bad := range []string{`{}`, `{"instructions":"x","tools":[{"name":""}]}`, `{"instructions":"x","tools":[{"name":"a","inputSchema":{"type":"object"}},{"name":"a","inputSchema":{"type":"object"}}]}`} {
		if _, err := parseCatalog([]byte(bad)); err == nil {
			t.Errorf("catalog %s accepted", bad)
		}
	}
	if string(ToolsJSON()) != string(toolsJSON) {
		t.Fatal("ToolsJSON")
	}
}

func TestEveryToolImplemented(t *testing.T) {
	if _, err := newHandler(Options{}); err != nil {
		t.Fatal(err)
	}
}
