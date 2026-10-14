package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/config"
)

// siteFiles is a fake ImageResolver: the site's uploads by URL, with calls
// recorded.
type siteFiles struct {
	mu    sync.Mutex
	files map[string][]byte // ref -> bytes
	errs  map[string]error  // ref -> error
	calls []resolveCall
}

type resolveCall struct {
	ref         string
	maxBytes    int64
	hasDeadline bool
}

func (s *siteFiles) resolve(ctx context.Context, ref string, maxBytes int64) ([]byte, error) {
	_, dl := ctx.Deadline()
	s.mu.Lock()
	s.calls = append(s.calls, resolveCall{ref: ref, maxBytes: maxBytes, hasDeadline: dl})
	s.mu.Unlock()
	if err, ok := s.errs[ref]; ok {
		return nil, err
	}
	if b, ok := s.files[ref]; ok {
		return b, nil
	}
	if strings.HasPrefix(ref, "/") {
		return nil, fmt.Errorf("%w: file not found", ai.ErrBadRequest)
	}
	return nil, ai.ErrNotLocal // another origin
}

func imageURL(u string) ai.Part { return ai.Part{Type: "image", URL: u} }

// TestResolveSiteImages: site-relative upload URLs (what
// backyard.files.upload returns) and absolute URLs of the site's own origin
// are loaded through the request's resolver and sent as inline bytes; other
// absolute URLs stay URLs for the provider / the SSRF-guarded fetcher.
func TestResolveSiteImages(t *testing.T) {
	const (
		rel     = "/__backyard/files/01JAAAAAAAAAAAAAAAAAAAAAAA/cat%20photo.png"
		pathRel = "/s/demo/__backyard/files/01JBBBBBBBBBBBBBBBBBBBBBBB/dog.png"
		own     = "https://demo.backyard.example/__backyard/files/01JCCCCCCCCCCCCCCCCCCCCCCC/c.png"
		foreign = "https://images.example.org/x.png"
	)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			f := newFixture(t, baseConfig())
			sf := &siteFiles{files: map[string][]byte{rel: pngBytes, pathRel: pngBytes, own: pngBytes}}
			parts := []ai.Part{
				{Type: "text", Text: "compare"}, imageURL(rel), imageURL(pathRel), imageURL(" " + rel + " "),
				imageURL(own), imageURL(foreign),
			}
			req := ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: parts}}, ResolveImage: sf.resolve}
			var err error
			if stream {
				_, err = f.svc.ChatStream(context.Background(), "smart", req, nil)
			} else {
				_, err = f.svc.Chat(context.Background(), "smart", req)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := f.claude.lastChat(t)
			if got.ResolveImage != nil {
				t.Error("the resolver reached the provider")
			}
			c := got.Messages[0].Content
			if len(c) != 6 {
				t.Fatalf("parts = %+v", c)
			}
			for _, i := range []int{1, 2, 3, 4} {
				if c[i].URL != "" || !bytes.Equal(c[i].Data, pngBytes) || c[i].MediaType != "image/png" {
					t.Errorf("part %d not resolved: %+v", i, c[i])
				}
			}
			if c[5].URL != foreign || c[5].Data != nil {
				t.Errorf("foreign URL part = %+v", c[5])
			}
			// Each distinct URL is offered once (the padded duplicate is the
			// same URL after trimming), in request order.
			var refs []string
			for _, call := range sf.calls {
				refs = append(refs, call.ref)
				if !call.hasDeadline {
					t.Errorf("resolver for %s ran without the request deadline", call.ref)
				}
			}
			if want := []string{rel, pathRel, own, foreign}; strings.Join(refs, " ") != strings.Join(want, " ") {
				t.Errorf("resolver calls = %v, want %v", refs, want)
			}
			if sf.calls[0].maxBytes != MaxImageBytes {
				t.Errorf("first maxBytes = %d, want %d", sf.calls[0].maxBytes, MaxImageBytes)
			}
			if sf.calls[1].maxBytes != MaxImageBytes {
				t.Errorf("second maxBytes = %d (a %d-byte image is far from the budget)", sf.calls[1].maxBytes, len(pngBytes))
			}
			// The caller's request is not modified.
			if parts[1].URL != rel || parts[1].Data != nil {
				t.Errorf("caller's part modified: %+v", parts[1])
			}
		})
	}
}

// TestSiteRelativeImageNeedsResolver: without a resolver (other callers of
// the router, the Cloudflare edition's contract) site-relative URLs keep
// being rejected.
func TestSiteRelativeImageNeedsResolver(t *testing.T) {
	f := newFixture(t, baseConfig())
	req := ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{imageURL("/__backyard/files/01J/a.png")}}}}
	_, err := f.svc.Chat(context.Background(), "smart", req)
	if !errors.Is(err, ai.ErrBadRequest) || !strings.Contains(err.Error(), "absolute http(s) URL") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveSiteImageErrors(t *testing.T) {
	errStorage := errors.New("blob store unavailable")
	big := make([]byte, MaxImageBytes+1)
	copy(big, pngBytes)
	tests := []struct {
		name      string
		url       string
		files     map[string][]byte
		errs      map[string]error
		role      string
		badReq    bool   // want ai.ErrBadRequest
		is        error  // want errors.Is(err, is)
		message   string // substring of err.Error()
		wantCalls int
	}{
		{name: "unknown upload", url: "/__backyard/files/01JX/a.png", badReq: true,
			message: "ai: bad request: message 0 part 1: file not found", wantCalls: 1},
		{name: "relative not served", url: "/assets/logo.png", errs: map[string]error{"/assets/logo.png": ai.ErrNotLocal},
			badReq: true, message: "does not refer to a file of this site", wantCalls: 1},
		{name: "storage failure", url: "/__backyard/files/01JX/a.png", errs: map[string]error{"/__backyard/files/01JX/a.png": errStorage},
			is: errStorage, message: "message 0 part 1: load image", wantCalls: 1},
		{name: "own-origin storage failure", url: "https://demo.example/__backyard/files/01JX/a.png",
			errs: map[string]error{"https://demo.example/__backyard/files/01JX/a.png": errStorage}, is: errStorage, wantCalls: 1},
		{name: "not an image", url: "/__backyard/files/01JX/a.png", files: map[string][]byte{"/__backyard/files/01JX/a.png": []byte("<svg onload=alert(1)>")},
			badReq: true, message: "not a png, jpeg, gif or webp image", wantCalls: 1},
		{name: "empty file", url: "/__backyard/files/01JX/a.png", files: map[string][]byte{"/__backyard/files/01JX/a.png": {}},
			badReq: true, message: "image is empty", wantCalls: 1},
		{name: "resolver ignores maxBytes", url: "/__backyard/files/01JX/a.png", files: map[string][]byte{"/__backyard/files/01JX/a.png": big},
			badReq: true, message: "image exceeds 20 MiB", wantCalls: 1},
		{name: "network-path reference", url: "//evil.example/a.png", badReq: true, message: "absolute http(s) URL", wantCalls: 0},
		{name: "backslash host trick", url: `/\evil.example/a.png`, badReq: true, message: "absolute http(s) URL", wantCalls: 0},
		{name: "file scheme", url: "file:///etc/passwd", badReq: true, message: "absolute http(s) URL", wantCalls: 0},
		{name: "assistant image", url: "/__backyard/files/01JX/a.png", role: "assistant", badReq: true,
			message: "only allowed in user messages", wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, baseConfig())
			sf := &siteFiles{files: tt.files, errs: tt.errs}
			role := tt.role
			if role == "" {
				role = "user"
			}
			req := ai.ChatRequest{
				Messages:     []ai.Message{{Role: role, Content: []ai.Part{{Type: "text", Text: "look"}, imageURL(tt.url)}}},
				ResolveImage: sf.resolve,
			}
			_, err := f.svc.Chat(context.Background(), "smart", req)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ai.ErrBadRequest); got != tt.badReq {
				t.Fatalf("errors.Is(ErrBadRequest) = %v, want %v (%v)", got, tt.badReq, err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("want %v in chain, got %v", tt.is, err)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error %q does not contain %q", err, tt.message)
			}
			if strings.Count(err.Error(), "bad request") > 1 {
				t.Fatalf("doubled prefix: %q", err)
			}
			if len(sf.calls) != tt.wantCalls {
				t.Fatalf("resolver called %d times, want %d", len(sf.calls), tt.wantCalls)
			}
			if n := len(f.claude.chats); n != 0 {
				t.Fatalf("failed request reached the provider %d times", n)
			}
		})
	}
}

// TestResolveTooManyImagesBeforeLoading: request-wide validation (the
// image count) runs before any upload is loaded.
func TestResolveTooManyImagesBeforeLoading(t *testing.T) {
	f := newFixture(t, baseConfig())
	sf := &siteFiles{files: map[string][]byte{}}
	parts := make([]ai.Part, 0, MaxImagesPerRequest+1)
	for i := range MaxImagesPerRequest + 1 {
		u := fmt.Sprintf("/__backyard/files/01J%023d/a.png", i)
		sf.files[u] = pngBytes
		parts = append(parts, imageURL(u))
	}
	_, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{
		Messages: []ai.Message{{Role: "user", Content: parts}}, ResolveImage: sf.resolve})
	if !errors.Is(err, ai.ErrBadRequest) || len(sf.calls) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(sf.calls))
	}
}

// TestResolveImageByteBudget: loaded uploads share MaxRemoteImageBytes with
// downloads, so a small request cannot make the server hold
// MaxImagesPerRequest × MaxImageBytes in memory; the resolver is told how
// much is left.
func TestResolveImageByteBudget(t *testing.T) {
	img := append(append([]byte{}, pngBytes...), make([]byte, MaxImageBytes-len(pngBytes))...) // exactly MaxImageBytes
	f := newFixture(t, baseConfig())
	sf := &siteFiles{files: map[string][]byte{}}
	var parts []ai.Part
	for i := range 4 {
		u := fmt.Sprintf("/__backyard/files/01J%023d/a.png", i)
		sf.files[u] = img // the fake ignores maxBytes on purpose
		parts = append(parts, imageURL(u))
	}
	_, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{
		Messages: []ai.Message{{Role: "user", Content: parts}}, ResolveImage: sf.resolve})
	if !errors.Is(err, ai.ErrBadRequest) || !strings.Contains(err.Error(), "message 0 part 3") ||
		!strings.Contains(err.Error(), fmt.Sprintf("exceed %d MiB per request", MaxRemoteImageBytes>>20)) {
		t.Fatalf("got %v", err)
	}
	if len(sf.calls) != 4 {
		t.Fatalf("calls = %d", len(sf.calls))
	}
	if got, want := sf.calls[3].maxBytes, int64(MaxRemoteImageBytes-3*MaxImageBytes); got != want {
		t.Fatalf("fourth maxBytes = %d, want %d", got, want)
	}

	// With the budget used up the resolver is not called at all.
	small := MaxRemoteImageBytes / 4 // four of these fill the budget exactly
	imgs := append(append([]byte{}, pngBytes...), make([]byte, small-len(pngBytes))...)
	sf = &siteFiles{files: map[string][]byte{}}
	parts = nil
	for i := range 5 {
		u := fmt.Sprintf("/__backyard/files/01K%023d/a.png", i)
		sf.files[u] = imgs
		parts = append(parts, imageURL(u))
	}
	_, err = f.svc.Chat(context.Background(), "smart", ai.ChatRequest{
		Messages: []ai.Message{{Role: "user", Content: parts}}, ResolveImage: sf.resolve})
	if !errors.Is(err, ai.ErrBadRequest) || len(sf.calls) != 4 {
		t.Fatalf("err=%v calls=%d", err, len(sf.calls))
	}
}

// TestResolvedImagesReduceDownloadBudget: what uploads used is no longer
// available to the provider's image downloads.
func TestResolvedImagesReduceDownloadBudget(t *testing.T) {
	remote := append(append([]byte{}, pngBytes...), make([]byte, imagefetch.MaxBytes-len(pngBytes)-1)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(remote)
	}))
	defer srv.Close()

	f := newFixture(t, baseConfig())
	var fetchErrs []error
	f.claude.chatFn = func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
		for range 3 {
			_, _, err := imagefetch.Get(ctx, srv.Client(), srv.URL+"/x.png")
			fetchErrs = append(fetchErrs, err)
		}
		return &ai.ChatResponse{Content: "ok", StopReason: ai.StopEndTurn}, nil
	}
	upload := append(append([]byte{}, pngBytes...), make([]byte, MaxImageBytes-len(pngBytes))...)
	sf := &siteFiles{files: map[string][]byte{"/__backyard/files/01JA/a.png": upload, "/__backyard/files/01JB/b.png": upload}}
	_, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{ResolveImage: sf.resolve, Messages: []ai.Message{{Role: "user",
		Content: []ai.Part{imageURL("/__backyard/files/01JA/a.png"), imageURL("/__backyard/files/01JB/b.png")}}}})
	if err != nil {
		t.Fatal(err)
	}
	// 64 MiB budget − 40 MiB of uploads leaves room for one 20 MiB download.
	ok := 0
	for _, err := range fetchErrs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, imagefetch.ErrBudgetExceeded):
			t.Errorf("unexpected fetch error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d downloads succeeded, want 1", ok)
	}
}

// TestResolveImageTimeout: loading uploads is bounded by the request
// timeout (reported like an upstream timeout) and honours cancellation.
func TestResolveImageTimeout(t *testing.T) {
	cfg := baseConfig()
	cfg.RequestTimeout = config.Duration(50 * time.Millisecond)
	f := newFixture(t, cfg)
	block := func(ctx context.Context, ref string, maxBytes int64) ([]byte, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("blob: %w", ctx.Err())
	}
	req := ai.ChatRequest{ResolveImage: block, Messages: []ai.Message{{Role: "user", Content: []ai.Part{imageURL("/__backyard/files/01JA/a.png")}}}}
	_, err := f.svc.Chat(context.Background(), "smart", req)
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusGatewayTimeout {
		t.Fatalf("want 504 upstream error, got %#v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err = f.svc.ChatStream(ctx, "smart", req, nil)
	if !errors.Is(err, context.Canceled) || errors.As(err, &ue) || errors.Is(err, ai.ErrBadRequest) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if n := len(f.claude.chats); n != 0 {
		t.Fatalf("provider called %d times", n)
	}
}
