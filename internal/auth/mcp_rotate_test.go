package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

// afterWrite runs its hook once, right after the first successful write of
// a grant's state (a create or an update; not a delete, not the audit log).
type afterWrite struct {
	docstore.Store
	mu   sync.Mutex
	hook func()
}

func (a *afterWrite) fire(collection string, err error) {
	if err != nil || collection == auditCollection {
		return
	}
	a.mu.Lock()
	f := a.hook
	a.hook = nil
	a.mu.Unlock()
	if f != nil {
		f()
	}
}

func (a *afterWrite) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	d, err := a.Store.Create(ctx, site, collection, id, data, actor)
	a.fire(collection, err)
	return d, err
}

func (a *afterWrite) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	d, err := a.Store.Update(ctx, site, collection, id, patch, actor)
	a.fire(collection, err)
	return d, err
}

func (a *afterWrite) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	d, err := a.Store.UpdateIf(ctx, site, collection, id, rev, patch, actor)
	a.fire(collection, err)
	return d, err
}

// Two refreshes of one token race (a proactive refresh A and a 401 retry
// B). Right after A's rotation lands, before A answers, B gets the grace
// answer RT(3) and the client refreshes that one too (C). A still answers
// RT(3), the connection survives and RT(4) refreshes.
func TestMCPRefreshRaceKeepsTheConnection(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		f := newFlow(e.register("App", loopbackCB), loopbackCB)
		_, r1 := e.tokens(newBrowser(), f)
		r2 := jsonBody(t, e.refresh(f, r1, nil))["refresh_token"].(string)
		e.m.waitBackground() // the prune of the consent reads m.docs
		var r3, r4 string
		e.m.docs = &afterWrite{Store: e.m.docs, hook: func() {
			b := e.refresh(f, r2, nil)
			if b.StatusCode != http.StatusOK {
				t.Errorf("B (RT(2), grace) = %d %s", b.StatusCode, body(t, b))
				return
			}
			r3 = jsonBody(t, b)["refresh_token"].(string)
			c := e.refresh(f, r3, nil)
			if c.StatusCode != http.StatusOK {
				t.Errorf("C (RT(3)) = %d %s", c.StatusCode, body(t, c))
				return
			}
			r4 = jsonBody(t, c)["refresh_token"].(string)
		}}
		a := e.refresh(f, r2, nil)
		if a.StatusCode != http.StatusOK {
			t.Fatalf("A (RT(2)) = %d %s; audit %v", a.StatusCode, body(t, a), e.auditActions())
		}
		if got := jsonBody(t, a)["refresh_token"]; got != r3 || r3 == "" {
			t.Fatalf("A = %v, B = %v; want the same RT(3)", got, r3)
		}
		if e.grantCount() != 1 {
			t.Fatal("the race revoked the connection")
		}
		if res := e.refresh(f, r4, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("RT(4) = %d %s", res.StatusCode, body(t, res))
		}
	})
}
