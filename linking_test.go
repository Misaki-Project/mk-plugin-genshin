package genshin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/plugin"
	"github.com/shiroha-a/mk/plugin/plugintest"
)

type linkingAPI struct{ limit atomic.Int64 }

func (a *linkingAPI) Anonymous() plugin.Caller    { return a }
func (a *linkingAPI) AsUser(string) plugin.Caller { return a }
func (a *linkingAPI) Call(_ context.Context, endpoint string, _ any) (json.RawMessage, error) {
	if endpoint != "i" {
		return nil, fmt.Errorf("unexpected endpoint: %s", endpoint)
	}
	return json.RawMessage(fmt.Sprintf(`{"host":null,"policies":{"genshinUidLimit":%d}}`, a.limit.Load())), nil
}

type verificationFixture struct {
	db    *sql.DB
	h     plugintest.Handlers
	api   *linkingAPI
	calls atomic.Int64
	match atomic.Bool
}

func newVerificationFixture(t *testing.T, limit int64) *verificationFixture {
	t.Helper()
	f := &verificationFixture{db: testDB(t), api: &linkingAPI{}}
	f.api.limit.Store(limit)
	f.match.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var signature string
		if f.match.Load() {
			if err := f.db.QueryRow(`SELECT string_agg(code,' ') FROM link_challenges WHERE uid=$1`, strings.TrimPrefix(r.URL.Path, "/api/uid/")).Scan(&signature); err != nil {
				http.Error(w, "missing proof", 404)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"playerInfo": map[string]any{"nickname": "Traveler", "signature": signature}, "ttl": 300})
	}))
	t.Cleanup(srv.Close)
	f.h = plugintest.New(t).WithName("genshin").WithDB(f.db).WithAPI(f.api).
		WithConfig(map[string]any{"endpoint": srv.URL, "timeoutSeconds": 5}).Routes(Plugin)
	return f
}

func (f *verificationFixture) begin(t *testing.T, user, uid string) challenge {
	t.Helper()
	res, err := f.h.Call(t, "POST /me/begin", plugintest.Request{UserID: user, Body: fmt.Sprintf(`{"uid":%q}`, uid)})
	if err != nil {
		t.Fatal(err)
	}
	return res.(challenge)
}
func (f *verificationFixture) verify(t *testing.T, user string, p challenge) (any, error) {
	t.Helper()
	return f.h.Call(t, "POST /me/verify", plugintest.Request{UserID: user, Body: fmt.Sprintf(`{"code":%q}`, p.Code)})
}
func (f *verificationFixture) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVerificationSuccessAndReplay(t *testing.T) {
	f := newVerificationFixture(t, 1)
	p := f.begin(t, "u1", "800000001")
	if remaining := time.Until(p.ExpiresAt); remaining < 9*time.Minute || remaining > 10*time.Minute {
		t.Fatalf("expiry: %s", remaining)
	}
	if f.count(t) != 0 {
		t.Fatal("pending request occupies a slot")
	}
	res, err := f.verify(t, "u1", p)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["verified"] != true || f.count(t) != 1 {
		t.Fatalf("not linked: %v", res)
	}
	if _, err = f.verify(t, "u1", p); err == nil {
		t.Fatal("used code accepted")
	}
	if _, err = f.h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000002"}`}); err == nil {
		t.Fatal("legacy bypass accepted")
	}
}

func TestVerificationTTLAndExpiry(t *testing.T) {
	f := newVerificationFixture(t, 1)
	f.match.Store(false)
	p := f.begin(t, "u1", "800000001")
	res, err := f.verify(t, "u1", p)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["verified"] != false {
		t.Fatal("nonmatching signature accepted")
	}
	f.match.Store(true)
	_, err = f.verify(t, "u1", p)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 || f.count(t) != 0 {
		t.Fatal("Enka ttl bypassed or unverified UID linked")
	}
	if _, err = f.db.Exec(`UPDATE link_challenges SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verify(t, "u1", p); err == nil {
		t.Fatal("expired code accepted")
	}
}

func TestVerificationReissueInvalidatesOldCode(t *testing.T) {
	f := newVerificationFixture(t, 2)
	p := f.begin(t, "u1", "800000001")
	if _, err := f.db.Exec(`UPDATE link_challenges SET issued_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	q := f.begin(t, "u1", "800000002")
	if p.Code == q.Code {
		t.Fatal("code reused")
	}
	if _, err := f.verify(t, "u1", p); err == nil {
		t.Fatal("old code accepted")
	}
	if _, err := f.verify(t, "u1", q); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationMultipleUIDsAndPolicyReduction(t *testing.T) {
	f := newVerificationFixture(t, 2)
	for _, uid := range []string{"800000001", "800000002"} {
		p := f.begin(t, "u1", uid)
		if _, err := f.verify(t, "u1", p); err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t) != 2 {
		t.Fatal("multiple verified UIDs not stored")
	}
	f.api.limit.Store(1)
	if _, err := f.h.Call(t, "POST /me/begin", plugintest.Request{UserID: "u1", Body: `{"uid":"800000003"}`}); err == nil {
		t.Fatal("lowered policy ignored")
	}
	if f.count(t) != 2 {
		t.Fatal("existing links removed on policy decrease")
	}
	f.api.limit.Store(2)
	p := f.begin(t, "u2", "800000003")
	f.api.limit.Store(0)
	if _, err := f.verify(t, "u2", p); err == nil {
		t.Fatal("policy not rechecked on verification")
	}
}

func TestVerificationUIDExclusivityAndUnlink(t *testing.T) {
	f := newVerificationFixture(t, 1)
	p := f.begin(t, "u1", "800000001")
	q := f.begin(t, "u2", "800000001")
	if _, err := f.verify(t, "u1", p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(t, "u2", q); err == nil {
		t.Fatal("UID linked to two users")
	}
	if _, err := f.h.Call(t, "POST /me/unlink", plugintest.Request{UserID: "u2", Body: `{"uid":"800000001"}`}); err != nil {
		t.Fatal(err)
	}
	if f.count(t) != 1 {
		t.Fatal("another user's link removed")
	}
	if _, err := f.h.Call(t, "POST /me/unlink", plugintest.Request{UserID: "u1", Body: `{"uid":"800000001"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(t, "u2", q); err != nil {
		t.Fatal(err)
	}
}

func TestLinkCodeEntropyAndBoundaries(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		code, err := newLinkCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 35 || seen[code] {
			t.Fatal("invalid/reused code")
		}
		seen[code] = true
		if !signatureHasCode("hello "+code+"!", code) || signatureHasCode(code+"x", code) || signatureHasCode(strings.ToUpper(code), code) {
			t.Fatal("unsafe token matching")
		}
	}
}

func TestVerificationRemoteCacheDoesNotCount(t *testing.T) {
	f := newVerificationFixture(t, 1)
	if _, err := f.db.Exec(`INSERT INTO peer_cache(host,key,payload,expires_at) VALUES('remote.example','u1','{"uid":"800000001"}',now()+interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	p := f.begin(t, "u1", "800000001")
	if _, err := f.verify(t, "u1", p); err != nil {
		t.Fatal(err)
	}
	if f.count(t) != 1 {
		t.Fatal("remote cache interfered with local limit")
	}
}

func TestVerificationParallelSameUID(t *testing.T) {
	f := newVerificationFixture(t, 1)
	p := f.begin(t, "u1", "800000001")
	q := f.begin(t, "u2", "800000001")
	errors := make(chan error, 2)
	go func() { _, err := f.verify(t, "u1", p); errors <- err }()
	go func() { _, err := f.verify(t, "u2", q); errors <- err }()
	successes := 0
	for range 2 {
		if <-errors == nil {
			successes++
		}
	}
	if successes != 1 || f.count(t) != 1 {
		t.Fatalf("duplicate UID race: successes=%d", successes)
	}
}

func TestVerificationAttemptLimit(t *testing.T) {
	f := newVerificationFixture(t, 1)
	p := f.begin(t, "u1", "800000001")
	if _, err := f.db.Exec(`UPDATE link_challenges SET attempts=10`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(t, "u1", p); err == nil {
		t.Fatal("attempt limit bypassed")
	}
	if f.calls.Load() != 0 {
		t.Fatal("attempt limit still requests Enka")
	}
}
