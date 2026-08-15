package genshin

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shiroha-a/mk/plugin/plugintest"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const testSchema = "plugin_genshin_test"

// testDB opens a throwaway schema for one test.
//
// フェイクの DB は使わない。SQL の挙動を模した偽物は本物とずれ、通ったのに
// 本番で落ちる形のテストになる。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`,
		`CREATE SCHEMA ` + testSchema,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

// setupRoutes wires the plugin against a throwaway schema and a fake Enka.
func setupRoutes(t *testing.T, enkaURL string) plugintest.Handlers {
	t.Helper()
	return plugintest.New(t).
		WithName("genshin").
		WithDB(testDB(t)).
		WithConfig(map[string]any{"endpoint": enkaURL, "userAgent": "test/1.0", "timeoutSeconds": 5}).
		Routes(Plugin)
}

func TestRoutes_SetAndShowProfile(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK,
		`{"playerInfo":{"nickname":"Traveler","level":60,"worldLevel":8,"signature":"hi"},"ttl":300}`)
	h := setupRoutes(t, srv.URL)

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`}); err != nil {
		t.Fatal(err)
	}

	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"u1"}`})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["linked"] != true || m["nickname"] != "Traveler" || m["adventureRank"] != 60 {
		t.Fatalf("想定と違う: %+v", m)
	}
}

// 未登録は「無い」であってエラーではない。表示側はこれを見て何も描かない。
func TestRoutes_ProfileOfUnlinkedUser(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{"playerInfo":{},"ttl":1}`)
	h := setupRoutes(t, srv.URL)

	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"nobody"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["linked"] != false {
		t.Fatalf("linked=false であるべき: %+v", res)
	}
}

func TestRoutes_RequiresLogin(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{}`)
	h := setupRoutes(t, srv.URL)

	for _, key := range []string{"POST /me", "POST /me/set"} {
		if _, err := h.Call(t, key, plugintest.Request{Body: `{"uid":"800000000"}`}); err == nil {
			t.Fatalf("%s: 未ログインを弾いていない", key)
		}
	}
}

// **存在しない UID を黙って保存しない。** プロフィールに何も出ない理由が
// 利用者に分からなくなる。
func TestRoutes_RejectsUnknownUID(t *testing.T) {
	srv := fakeEnka(t, http.StatusNotFound, `{}`)
	h := setupRoutes(t, srv.URL)

	_, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`})
	if err == nil {
		t.Fatal("エラーにならない")
	}
	if !strings.Contains(err.Error(), "見つかりません") {
		t.Fatalf("理由が伝わらない: %v", err)
	}
}

// 上流の一時的な不調では登録を拒まない (直るまで設定できないのは困る)。
func TestRoutes_SavesDespiteUpstreamOutage(t *testing.T) {
	srv := fakeEnka(t, http.StatusFailedDependency, `{"message":"game servers down"}`)
	h := setupRoutes(t, srv.URL)

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`}); err != nil {
		t.Fatalf("登録できるべき: %v", err)
	}

	res, err := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["uid"] != "800000000" {
		t.Fatalf("保存されていない: %+v", res)
	}
}

// 空文字で登録解除できること (UI から消せないと不便)。
func TestRoutes_UnlinkWithEmptyUID(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{"playerInfo":{"nickname":"x","level":1},"ttl":60}`)
	h := setupRoutes(t, srv.URL)

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":""}`}); err != nil {
		t.Fatal(err)
	}

	res, _ := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if res.(map[string]any)["uid"] != nil {
		t.Fatalf("解除されていない: %+v", res)
	}
}

func TestRoutes_RejectsBadUIDFormat(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{}`)
	h := setupRoutes(t, srv.URL)

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"abc"}`}); err == nil {
		t.Fatal("形式不正を弾いていない")
	}
}

// --- ジョブ ---

// 期限切れのものだけを取り直すこと。上流が落ちていても古いデータは消さない。
func TestJobs_RefreshKeepsStaleOnFailure(t *testing.T) {
	db := testDB(t)
	harness := plugintest.New(t).WithName("genshin").WithDB(db)

	ok := fakeEnka(t, http.StatusOK, `{"playerInfo":{"nickname":"Old","level":10},"ttl":-1}`)
	routes := harness.WithConfig(map[string]any{"endpoint": ok.URL, "timeoutSeconds": 5}).Routes(Plugin)
	if _, err := routes.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`}); err != nil {
		t.Fatal(err)
	}

	// 期限切れにしてから、上流が落ちている状態で更新を走らせる。
	if _, err := db.Exec(`UPDATE snapshots SET expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	down := fakeEnka(t, http.StatusFailedDependency, `{"message":"down"}`)
	jobs := plugintest.New(t).WithName("genshin").WithDB(db).
		WithConfig(map[string]any{"endpoint": down.URL, "timeoutSeconds": 5}).Jobs(Plugin)

	if err := jobs.Run(t, "refresh", ""); err != nil {
		t.Fatalf("1 件の失敗で全体を止めない: %v", err)
	}

	var nickname string
	if err := db.QueryRow(`SELECT nickname FROM snapshots WHERE uid = '800000000'`).Scan(&nickname); err != nil {
		t.Fatal(err)
	}
	if nickname != "Old" {
		t.Fatalf("上流が落ちていても古いデータを保持する: %q", nickname)
	}
}

// cron が登録されていること。
func TestJobs_RegistersSchedule(t *testing.T) {
	jobs := plugintest.New(t).WithName("genshin").WithDB(testDB(t)).Jobs(Plugin)

	if len(jobs.Schedules) != 1 || jobs.Schedules[0].Name != "refresh" {
		t.Fatalf("想定と違う: %+v", jobs.Schedules)
	}
}
