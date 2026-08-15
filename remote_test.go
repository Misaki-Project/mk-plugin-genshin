package genshin

import (
	"encoding/json"
	"strings"
	"testing"
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
