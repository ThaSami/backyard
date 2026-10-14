package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
)

// The grant store (SPEC §8.4, §10): one record per consent in
// _system/_mcp_grants/<gid>, plus a rotation marker per issued refresh
// generation in _system/_mcp_rotations/<gid>.<n>. The grant record is the
// single source of truth: every token check reads it, and revocation
// deletes it.

const (
	grantsCollection    = "_mcp_grants"
	rotationsCollection = "_mcp_rotations"
	auditCollection     = "_audit"
	storeTimeout        = 5 * time.Second
	prunePageGrants     = 100
	prunePageRotations  = 500
)

// Outcomes of rotate.
const (
	rotateOK      = "ok"
	rotateGrace   = "grace"
	rotateReuse   = "reuse"
	rotateMissing = "missing"
)

// grantUser is the user snapshot taken at consent.
type grantUser struct {
	ID        string
	Email     string
	Name      string
	Groups    []string
	Anonymous bool
}

// grantRecord is a stored grant (times are unix seconds; RedeemedAt 0 =
// not redeemed).
type grantRecord struct {
	ID            string
	User          grantUser
	ClientID      string
	ClientName    string
	ClientKind    string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
	CreatedAt     int64
	CodeExp       int64
	RedeemedAt    int64
	Exp           int64
}

// data returns the document body (the doc id is the gid).
func (g *grantRecord) data() map[string]any {
	groups := make([]any, len(g.User.Groups))
	for i, x := range g.User.Groups {
		groups[i] = x
	}
	d := map[string]any{
		"user_id": g.User.ID,
		"user": map[string]any{
			"id": g.User.ID, "email": g.User.Email, "name": g.User.Name,
			"groups": groups, "anonymous": g.User.Anonymous,
		},
		"client_id":      g.ClientID,
		"client_name":    g.ClientName,
		"client_kind":    g.ClientKind,
		"redirect_uri":   g.RedirectURI,
		"code_challenge": g.CodeChallenge,
		"scope":          g.Scope,
		"resource":       g.Resource,
		"created_at":     g.CreatedAt,
		"code_exp":       g.CodeExp,
		"exp":            g.Exp,
	}
	if g.RedeemedAt > 0 {
		d["redeemed_at"] = g.RedeemedAt
	}
	return d
}

// grantFromDoc decodes a stored grant.
func grantFromDoc(d *docstore.Doc) (*grantRecord, error) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	g := &grantRecord{
		ID:            d.ID,
		ClientID:      str(d.Data, "client_id"),
		ClientName:    str(d.Data, "client_name"),
		ClientKind:    str(d.Data, "client_kind"),
		RedirectURI:   str(d.Data, "redirect_uri"),
		CodeChallenge: str(d.Data, "code_challenge"),
		Scope:         str(d.Data, "scope"),
		Resource:      str(d.Data, "resource"),
		CreatedAt:     intValue(d.Data["created_at"]),
		CodeExp:       intValue(d.Data["code_exp"]),
		RedeemedAt:    intValue(d.Data["redeemed_at"]),
		Exp:           intValue(d.Data["exp"]),
	}
	u, _ := d.Data["user"].(map[string]any)
	g.User = grantUser{ID: str(u, "id"), Email: str(u, "email"), Name: str(u, "name")}
	g.User.Anonymous, _ = u["anonymous"].(bool)
	if list, ok := u["groups"].([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				g.User.Groups = append(g.User.Groups, s)
			}
		}
	}
	if g.User.ID == "" || g.ClientID == "" || g.CreatedAt == 0 || g.Exp == 0 {
		return nil, fmt.Errorf("auth: malformed mcp grant %s", d.ID)
	}
	return g, nil
}

// intValue reads a stored number as int64 (0 when absent or not a number).
func intValue(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// live reports whether a grant is redeemed and unexpired at now.
func (g *grantRecord) live(now int64) bool { return g.RedeemedAt > 0 && g.Exp > now }

// public converts a record into the exported Grant.
func (g *grantRecord) public() Grant {
	return Grant{
		ID: g.ID, ClientID: g.ClientID, ClientName: g.ClientName, ClientKind: g.ClientKind,
		ClientHost: clientHost(g.ClientID), RedirectURI: g.RedirectURI, Scope: g.Scope, Resource: g.Resource,
		User: identity.User{
			ID: g.User.ID, Email: g.User.Email, Name: g.User.Name,
			Groups: append([]string(nil), g.User.Groups...), Anonymous: g.User.Anonymous,
		},
		CreatedAt: time.Unix(g.CreatedAt, 0).UTC(),
		ExpiresAt: time.Unix(g.Exp, 0).UTC(),
	}
}

// identityUser rebuilds the user of the snapshot (for re-gating).
func (g *grantRecord) identityUser() *identity.User {
	return &identity.User{
		ID: g.User.ID, Email: g.User.Email, Name: g.User.Name,
		Groups: append([]string(nil), g.User.Groups...), Anonymous: g.User.Anonymous,
	}
}

// newGrantID returns 32 lowercase hex characters (16 random bytes).
func newGrantID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validGrantID reports whether id has the gid form.
func validGrantID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// storeCtx bounds a store call and keeps it running when the client goes
// away mid-transition.
func storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// createGrant stores a new pending grant and starts a background prune.
func (m *MCP) createGrant(ctx context.Context, g *grantRecord) error {
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	if _, err := m.docs.Create(sctx, docstore.SystemSite, grantsCollection, g.ID, g.data(), g.User.ID); err != nil {
		return err
	}
	m.startPrune()
	return nil
}

// loadGrant returns a grant, or nil, nil when it does not exist.
func (m *MCP) loadGrant(ctx context.Context, gid string) (*grantRecord, error) {
	if !validGrantID(gid) {
		return nil, nil
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	d, err := m.docs.Get(sctx, docstore.SystemSite, grantsCollection, gid)
	if errors.Is(err, docstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g, err := grantFromDoc(d)
	if err != nil {
		m.log.Warn("auth: ignoring a malformed mcp grant", "grant", shortGID(gid), "error", err)
		return nil, nil
	}
	return g, nil
}

// deleteGrant removes a grant; it reports whether it existed.
func (m *MCP) deleteGrant(ctx context.Context, gid string) (bool, error) {
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	return m.docs.Delete(sctx, docstore.SystemSite, grantsCollection, gid)
}

// rotate is the only state transition of a grant (SPEC §8.4.8). Code redemption
// calls it with from=0, grace=false; refreshing RT(n) with from=n,
// grace=true. It returns the outcome and, for ok and grace, the current
// grant (nil when the grant disappeared concurrently).
func (m *MCP) rotate(ctx context.Context, gid string, from int64, grace bool) (string, *grantRecord, error) {
	now := m.now()
	g, err := m.loadGrant(ctx, gid)
	if err != nil {
		return "", nil, err
	}
	if g == nil || g.Exp <= now.Unix() || (from >= 1 && g.RedeemedAt == 0) {
		return rotateMissing, nil, nil
	}
	// The family's exp is fixed at redemption (created_at + the token TTL
	// then in force) and never extended; refresh markers carry it as stored,
	// so a later, lower auth.token_ttl cannot prune them before the grant.
	familyExp := g.Exp
	if from == 0 {
		familyExp = g.CreatedAt + int64(m.family/time.Second)
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	marker := map[string]any{"gid": gid, "n": from + 1, "at": now.UnixMilli(), "exp": familyExp}
	_, err = m.docs.Create(sctx, docstore.SystemSite, rotationsCollection, markerID(gid, from+1), marker, g.User.ID)
	switch {
	case err == nil:
		if from == 0 {
			d, err := m.docs.Update(sctx, docstore.SystemSite, grantsCollection, gid,
				map[string]any{"redeemed_at": now.Unix(), "exp": familyExp}, g.User.ID)
			if errors.Is(err, docstore.ErrNotFound) {
				return rotateOK, nil, nil // revoked concurrently
			}
			if err != nil {
				return "", nil, err
			}
			if g, err = grantFromDoc(d); err != nil {
				return "", nil, err
			}
		}
		return rotateOK, g, nil
	case !errors.Is(err, docstore.ErrConflict):
		return "", nil, err
	}
	if grace {
		md, err := m.docs.Get(sctx, docstore.SystemSite, rotationsCollection, markerID(gid, from+1))
		if err != nil && !errors.Is(err, docstore.ErrNotFound) {
			return "", nil, err
		}
		if err == nil && now.UnixMilli()-intValue(md.Data["at"]) <= mcpRefreshGrace.Milliseconds() {
			_, err := m.docs.Get(sctx, docstore.SystemSite, rotationsCollection, markerID(gid, from+2))
			switch {
			case errors.Is(err, docstore.ErrNotFound):
				return rotateGrace, g, nil
			case err != nil:
				return "", nil, err
			}
		}
	}
	if _, err := m.docs.Delete(sctx, docstore.SystemSite, grantsCollection, gid); err != nil {
		return "", nil, err
	}
	return rotateReuse, g, nil
}

// markerID is the document id of rotation marker n of a grant.
func markerID(gid string, n int64) string { return gid + "." + strconv.FormatInt(n, 10) }

// startPrune deletes expired grants and rotation markers in the background
// (best effort, one prune at a time).
func (m *MCP) startPrune() {
	select {
	case m.pruneRunning <- struct{}{}:
	default:
		return
	}
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		defer func() { <-m.pruneRunning }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.prune(ctx)
	}()
}

// prune deletes one page of expired grants and one of expired markers.
func (m *MCP) prune(ctx context.Context) {
	now := float64(m.now().Unix())
	for _, p := range []struct {
		coll  string
		limit int
	}{{grantsCollection, prunePageGrants}, {rotationsCollection, prunePageRotations}} {
		q := docstore.Query{Where: []docstore.Filter{{Field: "exp", Op: docstore.OpLt, Value: now}}, Limit: p.limit}
		if err := q.Normalize(p.limit); err != nil {
			return
		}
		page, err := m.docs.Query(ctx, docstore.SystemSite, p.coll, q)
		if err != nil {
			m.log.Debug("auth: mcp prune query failed", "collection", p.coll, "error", err)
			continue
		}
		for _, d := range page.Docs {
			if _, err := m.docs.Delete(ctx, docstore.SystemSite, p.coll, d.ID); err != nil {
				m.log.Debug("auth: mcp prune delete failed", "collection", p.coll, "error", err)
			}
		}
	}
}

// waitBackground waits for background pruning (tests).
func (m *MCP) waitBackground() { m.bg.Wait() }

// userGrants returns grant records of userID (newest first); live selects
// redeemed, unexpired grants only.
func (m *MCP) userGrants(ctx context.Context, userID string, live bool, limit int) ([]*grantRecord, error) {
	q := docstore.Query{
		Where:   []docstore.Filter{{Field: "user_id", Op: docstore.OpEq, Value: userID}},
		OrderBy: []docstore.Order{{Field: "created_at", Dir: "desc"}},
		Limit:   limit,
	}
	if live {
		now := float64(m.now().Unix())
		q.Where = append(q.Where,
			docstore.Filter{Field: "exp", Op: docstore.OpGt, Value: now},
			docstore.Filter{Field: "redeemed_at", Op: docstore.OpGt, Value: float64(0)})
	}
	if err := q.Normalize(limit); err != nil {
		return nil, err
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	page, err := m.docs.Query(sctx, docstore.SystemSite, grantsCollection, q)
	if err != nil {
		return nil, err
	}
	out := make([]*grantRecord, 0, len(page.Docs))
	nowUnix := m.now().Unix()
	for _, d := range page.Docs {
		g, err := grantFromDoc(d)
		if err != nil || (live && !g.live(nowUnix)) {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

// listGrants implements MCP.Grants.
func (m *MCP) listGrants(ctx context.Context, userID string) ([]Grant, error) {
	if userID == "" {
		return []Grant{}, nil
	}
	recs, err := m.userGrants(ctx, userID, true, maxGrantList)
	if err != nil {
		return nil, err
	}
	out := make([]Grant, 0, len(recs))
	for _, g := range recs {
		out = append(out, g.public())
	}
	return out, nil
}

// revokeGrant deletes one grant and audits it (r supplies the client IP when
// known). actor nil means the grant's own user.
func (m *MCP) revokeGrant(ctx context.Context, r *http.Request, gid string, actor *identity.User, via string) (bool, error) {
	g, err := m.loadGrant(ctx, gid)
	if err != nil || g == nil {
		return false, err
	}
	ok, err := m.deleteGrant(ctx, gid)
	if err != nil || !ok {
		return false, err
	}
	m.audit(ctx, r, slog.LevelInfo, "mcp.revoke", actorID(actor, g), g, true, "via", via)
	return true, nil
}

// revokeUserGrants implements MCP.RevokeUser: it deletes every grant of
// userID (pending ones included) and returns how many it deleted.
func (m *MCP) revokeUserGrants(ctx context.Context, userID string, actor *identity.User, via string) (int, error) {
	if userID == "" {
		return 0, nil
	}
	n := 0
	for {
		recs, err := m.userGrants(ctx, userID, false, 500)
		if err != nil {
			return n, err
		}
		deleted := 0
		for _, g := range recs {
			ok, err := m.deleteGrant(ctx, g.ID)
			if err != nil {
				return n, err
			}
			if ok {
				deleted++
			}
		}
		n += deleted
		if len(recs) < 500 || deleted == 0 {
			break
		}
	}
	who := userID
	if actor != nil {
		who = actor.Actor()
	}
	m.audit(ctx, nil, slog.LevelInfo, "mcp.revoke", who, nil, true, "via", via, "count", n, "subject", userID)
	return n, nil
}

// actorID returns the id recorded as the actor of an event about g.
func actorID(actor *identity.User, g *grantRecord) string {
	if actor != nil {
		return actor.Actor()
	}
	if g != nil && g.User.ID != "" {
		return g.User.ID
	}
	return "unknown"
}

// shortGID is the grant label of logs and audit records (never the full id).
func shortGID(gid string) string {
	if len(gid) > 8 {
		return gid[:8]
	}
	return gid
}

// redirectHost is the host of a redirect URI (audit, hub card).
func redirectHost(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// audit logs an mcp.* event and, when store is set, records it in
// _system/_audit as {action, user, at, ip?, …fields}. g, when not nil,
// contributes the client and grant fields; r, when not nil, the client IP
// (else WithClientIP's address on ctx).
// Values never contain tokens, codes or full grant ids.
func (m *MCP) audit(ctx context.Context, r *http.Request, level slog.Level, action, user string, g *grantRecord, store bool, kv ...any) {
	fields := map[string]any{}
	if g != nil {
		fields["client_name"] = g.ClientName
		fields["client_kind"] = g.ClientKind
		fields["client_host"] = clientHost(g.ClientID)
		fields["redirect_host"] = redirectHost(g.RedirectURI)
		if g.ID != "" {
			fields["grant"] = shortGID(g.ID)
		}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			fields[k] = kv[i+1]
		}
	}
	ip := ""
	if r != nil {
		ip = m.a.clientIP(r)
	} else if v, ok := ctx.Value(clientIPKey{}).(string); ok && v != "unknown" {
		ip = v
	}
	attrs := []any{"audit", true, "action", action, "user", user}
	if ip != "" {
		attrs = append(attrs, "ip", ip)
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		attrs = append(attrs, k, fields[k])
	}
	m.log.Log(ctx, level, "audit", attrs...)
	if !store {
		return
	}
	data := map[string]any{"action": action, "user": user, "at": m.now().UTC().Format(time.RFC3339Nano)}
	if ip != "" {
		data["ip"] = ip
	}
	for k, v := range fields {
		data[k] = v
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	if _, err := m.docs.Create(sctx, docstore.SystemSite, auditCollection, "", data, user); err != nil {
		m.log.Warn("audit record write failed", "action", action, "error", err)
	}
}
