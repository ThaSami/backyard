package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// TestMCPCommandOverStreams drives `backyard mcp` through the command's own
// streams like an agent would: the JSON-RPC handshake, tools/list, a docs
// call with the real embedded skill and a tool error.
func TestMCPCommandOverStreams(t *testing.T) {
	h := newHarness(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	streams := IOStreams{In: inR, Out: outW, ErrOut: &h.err}
	done := make(chan int, 1)
	go func() {
		done <- execute(context.Background(), newRootCommand(streams, h.deps()), streams, []string{"mcp"})
		outW.Close()
	}()
	lines := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				m = map[string]any{"invalid": sc.Text()}
			}
			lines <- m
		}
		close(lines)
	}()
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case m, ok := <-lines:
				if !ok {
					t.Fatalf("%s: server exited; stderr: %s", method, h.err.String())
				}
				if bad, ok := m["invalid"]; ok {
					t.Fatalf("stdout is not pure JSON-RPC: %q", bad)
				}
				if got, ok := m["id"].(float64); ok && int(got) == id {
					return m
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: no reply", method)
			}
		}
	}
	call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"}})
	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"))
	list := call(2, "tools/list", map[string]any{})
	if tools, _ := list["result"].(map[string]any)["tools"].([]any); len(tools) != 8 {
		t.Fatalf("tools/list = %v", list)
	}
	docs, _ := json.Marshal(call(3, "tools/call", map[string]any{"name": "backyard_docs", "arguments": map[string]any{"topic": "api"}})["result"])
	if strings.Contains(string(docs), `"isError":true`) || !strings.Contains(string(docs), "backyard.db") {
		t.Fatalf("docs = %.300s", docs)
	}
	sites, _ := json.Marshal(call(4, "tools/call", map[string]any{"name": "backyard_list_sites", "arguments": map[string]any{}})["result"])
	if !strings.Contains(string(sites), `"isError":true`) || !strings.Contains(string(sites), "backyard login") {
		t.Fatalf("list_sites = %s", sites)
	}
	inW.Close() // the agent disconnects: the server exits
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("mcp did not exit after stdin closed")
	}
}
