package servertest

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/server"
)

// TestWebsocketOriginsWithRealtimeHub checks the allowed origins the server
// hands to the real realtime hub, in host and path mode, with and without
// realtime.allow_cross_site_ws.
func TestWebsocketOriginsWithRealtimeHub(t *testing.T) {
	type dial struct {
		host, path, origin string
		ok                 bool
	}
	tests := []struct {
		name      string
		hostMode  bool
		crossSite bool
		dials     []dial
	}{
		{"host mode", true, false, []dial{
			{"foo", "/", "http://foo.backyard.test:PORT", true},
			{"foo", "/", "", true},
			{"foo", "/", "http://bar.backyard.test:PORT", false},
			{"foo", "/", "http://backyard.test:PORT", false},
			{"foo", "/", "http://foo.backyard.test:1", false},
			{"foo", "/", "http://evil.example", false},
		}},
		{"host mode cross-site", true, true, []dial{
			{"foo", "/", "http://foo.backyard.test:PORT", true},
			{"foo", "/", "http://bar.backyard.test:PORT", true},
			{"foo", "/", "http://backyard.test:PORT", true},
			{"foo", "/", "http://www.backyard.test:PORT", false},
			{"foo", "/", "http://bar.backyard.test:1", false},
			{"foo", "/", "http://bar.backyard.test.evil.example:PORT", false},
			{"foo", "/", "null", false},
		}},
		{"path mode", false, false, []dial{
			{"", "/s/foo/", "http://backyard.test:PORT", true},
			{"", "/s/foo/", "", true},
			{"", "/s/foo/", "http://bar.backyard.test:PORT", false},
			{"", "/s/foo/", "http://evil.example", false},
		}},
		{"path mode cross-site", false, true, []dial{
			{"", "/s/foo/", "http://backyard.test:PORT", true},
			{"", "/s/foo/", "http://bar.backyard.test:PORT", false},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var port string
			in := newInstance(t, t.TempDir(), openSQLite(t), memory.New(), func(cfg *config.Config, _ *server.Deps) {
				u, err := url.Parse(cfg.Server.PublicURL)
				if err != nil {
					t.Fatal(err)
				}
				port = u.Port()
				cfg.Server.BaseDomain = "backyard.test"
				cfg.Server.PublicURL = "http://backyard.test:" + port
				if tc.hostMode {
					cfg.Server.URLMode = "host"
				}
				cfg.Realtime.AllowCrossSiteWS = tc.crossSite
			})
			for _, d := range tc.dials {
				origin := strings.ReplaceAll(d.origin, "PORT", port)
				host := "backyard.test:" + port
				if d.host != "" {
					host = d.host + ".backyard.test:" + port
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				hdr := http.Header{}
				if origin != "" {
					hdr.Set("Origin", origin)
				}
				conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(in.url, "http")+strings.TrimSuffix(d.path, "/")+"/__backyard/api/ws",
					&websocket.DialOptions{Host: host, HTTPHeader: hdr, HTTPClient: &http.Client{}})
				cancel()
				if d.ok != (err == nil) {
					status := 0
					if resp != nil {
						status = resp.StatusCode
					}
					t.Errorf("Host %s, Origin %q: accepted=%v, want %v (status %d, err %v)", host, origin, err == nil, d.ok, status, err)
				}
				if conn != nil {
					_ = conn.Close(websocket.StatusNormalClosure, "")
				}
				if !d.ok && err != nil && resp != nil && resp.StatusCode != http.StatusForbidden {
					t.Errorf("Host %s, Origin %q: rejected with status %d, want 403", host, origin, resp.StatusCode)
				}
			}
		})
	}
}
