package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/realtime"
	"github.com/thasami/backyard/internal/warehouse"
)

// ---------------------------------------------------------------------------
// Blob store

type memObj struct {
	data []byte
	info blob.ObjectInfo
}

type memBlob struct {
	mu       sync.Mutex
	objs     map[string]*memObj
	stats    int
	gets     int
	ranges   int
	pingErr  error
	putErr   error
	delErr   error
	statHook func(key string) error
}

var _ blob.Store = (*memBlob)(nil)

func newMemBlob() *memBlob { return &memBlob{objs: map[string]*memObj{}} }

func (m *memBlob) set(key string, data []byte, mod time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := md5.Sum(data)
	m.objs[key] = &memObj{data: append([]byte(nil), data...), info: blob.ObjectInfo{
		Key: key, Size: int64(len(data)), ETag: `"` + hex.EncodeToString(sum[:]) + `"`, ModTime: mod,
	}}
}

func (m *memBlob) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
}

func (m *memBlob) bytesOf(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objs[key]; ok {
		return append([]byte(nil), o.data...)
	}
	return nil
}

func (m *memBlob) counts() (stats, gets, ranges int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats, m.gets, m.ranges
}

func (m *memBlob) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memBlob) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	m.mu.Lock()
	m.gets++
	m.mu.Unlock()
	return m.read(ctx, key, 0, -1)
}

func (m *memBlob) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	m.mu.Lock()
	m.ranges++
	m.mu.Unlock()
	return m.read(ctx, key, offset, length)
}

func (m *memBlob) read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, nil, blob.ErrNotFound
	}
	data := o.data
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	data = data[offset:]
	if length >= 0 && length < int64(len(data)) {
		data = data[:length]
	}
	info := o.info
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), &info, nil
}

func (m *memBlob) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.stats++
	hook := m.statHook
	m.mu.Unlock()
	if hook != nil {
		if err := hook(key); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	info := o.info
	return &info, nil
}

func (m *memBlob) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if !blob.ValidKey(key) {
		return nil, blob.ErrInvalidKey
	}
	if m.putErr != nil {
		return nil, m.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	m.set(key, data, time.Now())
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key].info.ContentType = contentType
	info := m.objs[key].info
	return &info, nil
}

func (m *memBlob) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objs, key)
	return nil
}

func (m *memBlob) DeletePrefix(ctx context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			delete(m.objs, k)
		}
	}
	return nil
}

func (m *memBlob) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := &blob.ListResult{}
	seen := map[string]bool{}
	for k, o := range m.objs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				p := prefix + rest[:i+len(delimiter)]
				if !seen[p] {
					seen[p] = true
					res.Prefixes = append(res.Prefixes, p)
				}
				continue
			}
		}
		res.Objects = append(res.Objects, o.info)
	}
	sort.Slice(res.Objects, func(i, j int) bool { return res.Objects[i].Key < res.Objects[j].Key })
	sort.Strings(res.Prefixes)
	return res, nil
}

func (m *memBlob) PresignPut(context.Context, string, blob.PresignPutOptions, time.Duration) (*blob.PresignedRequest, error) {
	return nil, blob.ErrNotSupported
}

func (m *memBlob) PresignGet(context.Context, string, time.Duration) (*blob.PresignedRequest, error) {
	return nil, blob.ErrNotSupported
}

func (m *memBlob) Ping(ctx context.Context) error { return m.pingErr }
func (m *memBlob) Describe() blob.Description     { return blob.Description{Type: "mem"} }
func (m *memBlob) Close() error                   { return nil }

// ---------------------------------------------------------------------------
// Document store

type memDocs struct {
	mu      sync.Mutex
	docs    map[string]map[string]map[string]*docstore.Doc // site → coll → id
	errs    map[string]error                               // op → injected error
	pingErr error
	clock   time.Time
}

var _ docstore.Store = (*memDocs)(nil)

func newMemDocs() *memDocs {
	return &memDocs{docs: map[string]map[string]map[string]*docstore.Doc{}, errs: map[string]error{},
		clock: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (m *memDocs) fail(op string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs[op] = err
}

func (m *memDocs) injected(op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errs[op]
}

func (m *memDocs) tick() time.Time {
	m.clock = m.clock.Add(time.Second)
	return m.clock
}

func (m *memDocs) coll(site, c string, create bool) map[string]*docstore.Doc {
	s := m.docs[site]
	if s == nil {
		if !create {
			return nil
		}
		s = map[string]map[string]*docstore.Doc{}
		m.docs[site] = s
	}
	cc := s[c]
	if cc == nil && create {
		cc = map[string]*docstore.Doc{}
		s[c] = cc
	}
	return cc
}

func (m *memDocs) Create(ctx context.Context, site, c, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if err := m.injected("create"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = docstore.NewID()
	}
	cc := m.coll(site, c, true)
	if _, ok := cc[id]; ok {
		return nil, docstore.ErrConflict
	}
	now := m.tick()
	d := &docstore.Doc{Site: site, Collection: c, ID: id, Data: docstore.CloneData(data),
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1}
	cc[id] = d
	return docstore.CloneDoc(d), nil
}

func (m *memDocs) Get(ctx context.Context, site, c, id string) (*docstore.Doc, error) {
	if err := m.injected("get"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.coll(site, c, false)[id]
	if !ok {
		return nil, docstore.ErrNotFound
	}
	return docstore.CloneDoc(d), nil
}

func (m *memDocs) Put(ctx context.Context, site, c, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if err := m.injected("put"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cc := m.coll(site, c, true)
	now := m.tick()
	d, ok := cc[id]
	if !ok {
		d = &docstore.Doc{Site: site, Collection: c, ID: id, CreatedAt: now, CreatedBy: actor}
		cc[id] = d
	}
	d.Data = docstore.CloneData(data)
	d.UpdatedAt, d.UpdatedBy = now, actor
	d.Version++
	return docstore.CloneDoc(d), nil
}

func (m *memDocs) Update(ctx context.Context, site, c, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	return m.update(site, c, id, nil, patch, actor)
}

func (m *memDocs) UpdateIf(ctx context.Context, site, c, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	return m.update(site, c, id, &rev, patch, actor)
}

func (m *memDocs) update(site, c, id string, rev *docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	if err := m.injected("update"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.coll(site, c, false)[id]
	if !ok {
		return nil, docstore.ErrNotFound
	}
	if rev != nil && !d.At(*rev) {
		return nil, docstore.ErrConflict
	}
	d.Data = docstore.ApplyPatch(d.Data, patch)
	d.UpdatedAt, d.UpdatedBy = m.tick(), actor
	d.Version++
	return docstore.CloneDoc(d), nil
}

func (m *memDocs) Delete(ctx context.Context, site, c, id string) (bool, error) {
	return m.delete(site, c, id, nil)
}

func (m *memDocs) DeleteIf(ctx context.Context, site, c, id string, rev docstore.Rev) (bool, error) {
	return m.delete(site, c, id, &rev)
}

func (m *memDocs) delete(site, c, id string, rev *docstore.Rev) (bool, error) {
	if err := m.injected("delete"); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cc := m.coll(site, c, false)
	if d, ok := cc[id]; !ok || (rev != nil && !d.At(*rev)) {
		return false, nil
	}
	delete(cc, id)
	return true, nil
}

func (m *memDocs) Query(ctx context.Context, site, c string, q docstore.Query) (*docstore.Page, error) {
	if err := m.injected("query"); err != nil {
		return nil, err
	}
	pq, err := docstore.PrepareQuery(q)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*docstore.Doc
	for _, d := range m.coll(site, c, false) {
		all = append(all, docstore.CloneDoc(d))
	}
	return docstore.EvalQuery(all, pq), nil
}

func (m *memDocs) Collections(ctx context.Context, site string) ([]string, error) {
	if err := m.injected("collections"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for c, docs := range m.docs[site] {
		if len(docs) > 0 {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (m *memDocs) Count(ctx context.Context, site string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, docs := range m.docs[site] {
		n += int64(len(docs))
	}
	return n, nil
}

func (m *memDocs) DeleteSite(ctx context.Context, site string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, site)
	return nil
}

func (m *memDocs) Ping(ctx context.Context) error { return m.pingErr }
func (m *memDocs) Close() error                   { return nil }

func (m *memDocs) collectionLen(site, c string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.coll(site, c, false))
}

// ---------------------------------------------------------------------------
// Authenticator

type fakeAuth struct {
	mu        sync.Mutex
	user      *identity.User
	err       error
	loginBase string // "" → LoginURL returns ""
	calls     int
	handled   []string
	handler   http.Handler
}

func (a *fakeAuth) Authenticate(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if a.user == nil {
		return nil, errors.New("fakeAuth: no user configured")
	}
	return a.user.Clone(), nil
}

func (a *fakeAuth) LoginURL(returnTo string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loginBase == "" {
		return ""
	}
	return a.loginBase + "?rd=" + returnTo
}

func (a *fakeAuth) Handler() http.Handler {
	if a.handler != nil {
		return a.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.handled = append(a.handled, r.URL.Path)
		a.mu.Unlock()
		_, _ = io.WriteString(w, "auth handler "+r.URL.Path)
	})
}

func (a *fakeAuth) Mode() string { return "oidc" }

func (a *fakeAuth) set(u *identity.User, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.user, a.err = u, err
}

// ---------------------------------------------------------------------------
// Realtime hub

type wsCall struct {
	site    string
	user    *identity.User
	origins []string
}

type fakeHub struct {
	mu     sync.Mutex
	events []pubEvent
	ws     []wsCall
	conns  int
}

type pubEvent struct {
	site string
	ev   realtime.DBEvent
}

func (h *fakeHub) ServeWS(w http.ResponseWriter, r *http.Request, site string, user *identity.User, allowedOrigins []string) {
	h.mu.Lock()
	h.ws = append(h.ws, wsCall{site: site, user: user, origins: append([]string(nil), allowedOrigins...)})
	h.mu.Unlock()
	w.WriteHeader(http.StatusTeapot) // marker
}

func (h *fakeHub) PublishDB(ctx context.Context, site string, ev realtime.DBEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, pubEvent{site: site, ev: ev})
	return nil
}

func (h *fakeHub) Stats() realtime.Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return realtime.Stats{Connections: h.conns}
}

func (h *fakeHub) Close() error { return nil }

func (h *fakeHub) published() []pubEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pubEvent(nil), h.events...)
}

func (h *fakeHub) wsCalls() []wsCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wsCall(nil), h.ws...)
}

// ---------------------------------------------------------------------------
// AI service

type fakeAI struct {
	mu        sync.Mutex
	enabled   bool
	chatReqs  []ai.ChatRequest
	aliases   []string
	chatResp  *ai.ChatResponse
	chatErr   error
	deltas    []string
	streamErr error
	images    []ai.Image
	imageErr  error
	embedErr  error
	models    []ai.ModelInfo
	noDefault bool          // Models reports no default chat model
	delay     time.Duration // before the first stream delta
}

func newFakeAI() *fakeAI {
	return &fakeAI{
		enabled:  true,
		chatResp: &ai.ChatResponse{Content: "hello", Model: "smart", StopReason: ai.StopEndTurn, Usage: ai.Usage{InputTokens: 12, OutputTokens: 40}},
		deltas:   []string{"Hel", "lo", " world"},
		images:   []ai.Image{{Data: []byte("\x89PNG\r\n\x1a\nfake"), MediaType: "image/png"}},
		models: []ai.ModelInfo{
			{Name: "embed", Kind: "embedding", Provider: "openai"},
			{Name: "image", Kind: "image", Provider: "openai"},
			{Name: "smart", Kind: "chat", Provider: "anthropic", Description: "Best"},
		},
	}
}

func (f *fakeAI) record(alias string, req ai.ChatRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliases = append(f.aliases, alias)
	f.chatReqs = append(f.chatReqs, req)
}

func (f *fakeAI) lastChat() (string, ai.ChatRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.chatReqs) == 0 {
		return "", ai.ChatRequest{}
	}
	return f.aliases[len(f.aliases)-1], f.chatReqs[len(f.chatReqs)-1]
}

func (f *fakeAI) Chat(ctx context.Context, alias string, req ai.ChatRequest) (*ai.ChatResponse, error) {
	f.record(alias, req)
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	r := *f.chatResp
	if alias != "" {
		r.Model = alias
	}
	return &r, nil
}

func (f *fakeAI) ChatStream(ctx context.Context, alias string, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	f.record(alias, req)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var full strings.Builder
	for _, d := range f.deltas {
		if err := onDelta(d); err != nil {
			return nil, err
		}
		full.WriteString(d)
	}
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	model := alias
	if model == "" {
		model = "smart"
	}
	return &ai.ChatResponse{Content: full.String(), Model: model, StopReason: ai.StopEndTurn, Usage: ai.Usage{InputTokens: 5, OutputTokens: 7}}, nil
}

func (f *fakeAI) Image(ctx context.Context, alias string, req ai.ImageRequest) (*ai.ImageResponse, string, error) {
	if f.imageErr != nil {
		return nil, "", f.imageErr
	}
	if alias == "" {
		alias = "image"
	}
	var imgs []ai.Image
	for range max(req.N, 1) {
		imgs = append(imgs, f.images...)
	}
	return &ai.ImageResponse{Images: imgs}, alias, nil
}

func (f *fakeAI) Embed(ctx context.Context, alias string, req ai.EmbedRequest) (*ai.EmbedResponse, string, error) {
	if f.embedErr != nil {
		return nil, "", f.embedErr
	}
	if alias == "" {
		alias = "embed"
	}
	out := &ai.EmbedResponse{Usage: ai.Usage{InputTokens: len(req.Input)}}
	for i := range req.Input {
		out.Embeddings = append(out.Embeddings, []float64{float64(i), 0.5})
	}
	return out, alias, nil
}

func (f *fakeAI) Models() (string, []ai.ModelInfo) {
	if f.noDefault {
		return "", f.models
	}
	return "smart", f.models
}
func (f *fakeAI) Enabled() bool { return f.enabled }

// ---------------------------------------------------------------------------
// Warehouse

type fakeWarehouse struct {
	mu      sync.Mutex
	enabled bool
	reqs    []warehouse.QueryRequest
	res     *warehouse.Result
	err     error
}

func (f *fakeWarehouse) Query(ctx context.Context, req warehouse.QueryRequest) (*warehouse.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func (f *fakeWarehouse) Describe() ([]warehouse.SourceInfo, []warehouse.QueryInfo) {
	return []warehouse.SourceInfo{{Name: "default", Type: "postgres"}},
		[]warehouse.QueryInfo{{Name: "wau", Description: "weekly", Source: "default"}}
}

func (f *fakeWarehouse) Enabled() bool { return f.enabled }
func (f *fakeWarehouse) Close() error  { return nil }

// ---------------------------------------------------------------------------
// Gateway

type fakeGateway struct {
	mu       sync.Mutex
	paths    []string
	status   int
	panicMsg string
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.Method+" "+r.URL.Path)
	status, p := g.status, g.panicMsg
	g.mu.Unlock()
	if p != "" {
		panic(p)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
}

func (g *fakeGateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.paths...)
}
