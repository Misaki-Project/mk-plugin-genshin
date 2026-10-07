package genshin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// 相手のインスタンスのプロキシ URL を、こちらのプロキシ URL に貼り替えること。
//
// **そのまま出すと 2 つ壊れる。** CSP (img-src 'self') で表示できないうえ、
// 閲覧者の接続先が相手のサーバーに漏れる。
func TestRewriteAssetURL(t *testing.T) {
	got := rewriteAssetURL("https://other.example/api/plugin/genshin/asset/UI_AvatarIcon_Ayaka")
	if want := "/api/plugin/genshin/asset/UI_AvatarIcon_Ayaka"; got != want {
		t.Errorf("got %q want %q", got, want)
	}

	// 素の URL は触らない。
	if got := rewriteAssetURL("https://example.test/files/x.png"); got != "https://example.test/files/x.png" {
		t.Errorf("関係ない URL を書き換えた: %q", got)
	}

	// 想定外の名前は落とす。相手が渡した文字列をそのまま URL にしない。
	for _, bad := range []string{
		"https://other.example/api/plugin/genshin/asset/../../etc/passwd",
		"https://other.example/api/plugin/genshin/asset/",
		"https://other.example/api/plugin/genshin/asset/a b",
	} {
		if got := rewriteAssetURL(bad); got != "" {
			t.Errorf("不正な名前を通した: %q -> %q", bad, got)
		}
	}
}

// 入れ子になった応答の中の URL もすべて貼り替えること。
func TestRewriteAssetHosts_Nested(t *testing.T) {
	raw := `{
		"profileIcon": "https://other.example/api/plugin/genshin/asset/UI_A",
		"characters": [
			{"icon": "https://other.example/api/plugin/genshin/asset/UI_B",
			 "weapon": {"icon": "https://other.example/api/plugin/genshin/asset/UI_C"},
			 "artifacts": [{"icon": "https://other.example/api/plugin/genshin/asset/UI_D"}]}
		]
	}`
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	rewriteAssetHosts(v, "other.example")

	out, _ := json.Marshal(v)
	s := string(out)
	if strings.Contains(s, "other.example") {
		t.Errorf("相手のホストが残っている: %s", s)
	}
	for _, name := range []string{"UI_A", "UI_B", "UI_C", "UI_D"} {
		if !strings.Contains(s, "/api/plugin/genshin/asset/"+name) {
			t.Errorf("%s が貼り替わっていない: %s", name, s)
		}
	}
}

// fakeAPI records which caller the plugin used.
type fakeAPI struct {
	calls []string
	resp  json.RawMessage
	err   error
}

func (a *fakeAPI) Anonymous() plugin.Caller { return &fakeCaller{api: a, who: "anonymous"} }
func (a *fakeAPI) AsUser(userID string) plugin.Caller {
	return &fakeCaller{api: a, who: "asUser:" + userID}
}

type fakeCaller struct {
	api *fakeAPI
	who string
}

func (c *fakeCaller) Call(_ context.Context, endpoint string, _ any) (json.RawMessage, error) {
	c.api.calls = append(c.api.calls, c.who+" "+endpoint)
	return c.api.resp, c.api.err
}

// fakeCtx is the minimum plugin.Context remoteAcct needs.
type fakeCtx struct {
	plugin.Context
	api plugin.API
}

func (c *fakeCtx) API() plugin.API { return c.api }

// **閲覧者として引く。** 匿名で引くと ugcVisibilityForVisitor='local' (既定) の
// インスタンスでリモート利用者が NO_SUCH_USER になり、問い合わせ自体が出せない
// (mk-go #2106)。実際にこれで「いつまでも出ない」不具合を踏んだ。
func TestRemoteAcct_CallsAsViewer(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)}
	ctx := &fakeCtx{api: api}

	host, username, err := remoteAcct(context.Background(), ctx, "viewer1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "other.example" || username != "alice" {
		t.Fatalf("host/username: %q / %q", host, username)
	}
	if len(api.calls) != 1 || api.calls[0] != "asUser:viewer1 users/show" {
		t.Errorf("閲覧者として呼んでいない: %v", api.calls)
	}
}

// 未ログインの閲覧者では匿名で引く。引けないのは設定どおりの挙動なので、
// 無理に権限を上げない。
func TestRemoteAcct_AnonymousWhenNoViewer(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)}
	ctx := &fakeCtx{api: api}

	if _, _, err := remoteAcct(context.Background(), ctx, "", "u1"); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || api.calls[0] != "anonymous users/show" {
		t.Errorf("匿名で呼んでいない: %v", api.calls)
	}
}

// ローカル利用者には host が無い。問い合わせ先が無いので何もしない。
func TestRemoteAcct_LocalUser(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":null}`)}
	ctx := &fakeCtx{api: api}

	host, _, err := remoteAcct(context.Background(), ctx, "viewer1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		t.Errorf("ローカル利用者に host を返した: %q", host)
	}
}

// --- peer channel ---

// peerHarness wires the plugin's peer callback against a throwaway schema.
//
// **`h.Peer(Plugin)` を通す (mk-go #2819)。** 登録は Definition.Peer にあるので、
// Routes だけではハンドラが 1 つも入らない。
func peerHarness(t *testing.T, api plugin.API, seed ...bool) (*plugintest.Harness, plugintest.Handlers) {
	t.Helper()
	srv := fakeEnka(t, http.StatusOK,
		`{"playerInfo":{"nickname":"Traveler","level":60,"worldLevel":8,"signature":"hi"},"ttl":300}`)
	db := testDB(t)
	h := plugintest.New(t).
		WithName("genshin").
		WithDB(db).
		WithAPI(api).
		WithPeers("other.example").
		WithConfig(map[string]any{"endpoint": srv.URL, "userAgent": "test/1.0", "timeoutSeconds": 5})
	h.Peer(Plugin)
	if len(seed) > 0 && seed[0] {
		seedVerifiedSnapshot(t, db, srv.URL)
	}
	return h, h.Routes(Plugin)
}

// 相手から聞かれたら、自分のところの利用者の分だけ答える。
func TestPeer_AnswersForLocalUser(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u1","host":null}`)}
	h, _ := peerHarness(t, api, true)

	res, err := h.DeliverPeer("other.example", peerRequest{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(peerResponse)
	if !ok {
		t.Fatalf("応答の型が違う: %T", res)
	}
	if !got.Linked || len(got.Profile) == 0 {
		t.Fatalf("登録済みの利用者なのに linked が立っていない: %+v", got)
	}
}

// UID を登録していない利用者は linked=false。
func TestPeer_AnswersUnlinked(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u9","host":null}`)}
	h, _ := peerHarness(t, api)

	res, err := h.DeliverPeer("other.example", peerRequest{Username: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.(peerResponse); got.Linked {
		t.Fatalf("登録していないのに linked: %+v", got)
	}
}

// **リモート利用者を開いたら問い合わせが出る。** 初回は空で返る。
func TestRemoteLookup_AsksThenServesCache(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u2","host":"other.example","username":"alice"}`)}
	h, _ := peerHarness(t, api)
	ctx := h.Context()
	db := ctx.Storage().DB()

	got, err := remoteLookup(context.Background(), ctx, db, "viewer", "u2")
	if err != nil {
		t.Fatal(err)
	}
	if m := got.(map[string]any); m["linked"] != false {
		t.Fatalf("初回は空で返るはず: %+v", m)
	}

	sends := h.PeerSends()
	if len(sends) != 1 {
		t.Fatalf("問い合わせが 1 件出るはず: %+v", sends)
	}
	if sends[0].Host != "other.example" {
		t.Fatalf("宛先が違う: %+v", sends[0])
	}

	// 応答が届いたら次から出る。
	if err := h.DeliverPeerReply("other.example", sends[0].ID, peerResponse{
		Linked: true,
		Profile: json.RawMessage(
			`{"linked":true,"nickname":"Remote",` +
				`"profileIcon":"https://other.example/api/plugin/genshin/asset/UI_A"}`),
	}); err != nil {
		t.Fatal(err)
	}
	got, err = remoteLookup(context.Background(), ctx, db, "viewer", "u2")
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["nickname"] != "Remote" {
		t.Fatalf("キャッシュから返らない: %+v", m)
	}
	// **相手のホストを残さない。** そのまま出すと CSP で表示できないうえ、
	// 閲覧者の接続先が相手に漏れる。
	out, _ := json.Marshal(m)
	if strings.Contains(string(out), "other.example") {
		t.Errorf("相手のホストが残っている: %s", out)
	}
	if !strings.Contains(string(out), "/api/plugin/genshin/asset/UI_A") {
		t.Errorf("こちらのプロキシに貼り替わっていない: %s", out)
	}
}
