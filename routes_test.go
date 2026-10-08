package genshin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/plugin/plugintest"
	_ "github.com/jackc/pgx/v5/stdlib"
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
		WithAPI(defaultLinkingAPI()).
		WithConfig(map[string]any{"endpoint": enkaURL, "userAgent": "test/1.0", "timeoutSeconds": 5}).
		Routes(Plugin)
}

func TestRoutes_SetAndShowProfile(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK,
		`{"playerInfo":{"nickname":"Traveler","level":60,"worldLevel":8,"signature":"hi"},"ttl":300}`)
	db := testDB(t)
	h := plugintest.New(t).WithName("genshin").WithDB(db).WithAPI(defaultLinkingAPI()).WithConfig(map[string]any{"endpoint": srv.URL}).Routes(Plugin)
	seedVerifiedSnapshot(t, db, srv.URL)

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

	res, err := h.Call(t, "POST /me/begin", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`})
	if err != nil {
		t.Fatal(err)
	}
	p := res.(challenge)
	_, err = h.Call(t, "POST /me/verify", plugintest.Request{UserID: "u1", Body: fmt.Sprintf(`{"code":%q}`, p.Code)})
	if err == nil {
		t.Fatal("エラーにならない")
	}
	if !strings.Contains(err.Error(), "取得できません") {
		t.Fatalf("理由が伝わらない: %v", err)
	}
}

// 本人確認用の上流が停止中なら、未検証UIDは登録してはいけない。
func TestRoutes_DoesNotLinkDuringUpstreamOutage(t *testing.T) {
	srv := fakeEnka(t, http.StatusFailedDependency, `{"message":"game servers down"}`)
	h := setupRoutes(t, srv.URL)

	begin, err := h.Call(t, "POST /me/begin", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`})
	if err != nil {
		t.Fatal(err)
	}
	p := begin.(challenge)
	if _, err := h.Call(t, "POST /me/verify", plugintest.Request{UserID: "u1", Body: fmt.Sprintf(`{"code":%q}`, p.Code)}); err == nil {
		t.Fatal("未確認UIDを登録した")
	}

	res, err := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.(map[string]any)["uids"].([]string)) != 0 {
		t.Fatalf("未確認UIDが保存された: %+v", res)
	}
}

// 指定UIDだけを解除できること。
func TestRoutes_UnlinkUID(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{"playerInfo":{"nickname":"x","level":1},"ttl":60}`)
	db := testDB(t)
	h := plugintest.New(t).WithName("genshin").WithDB(db).WithAPI(defaultLinkingAPI()).Routes(Plugin)
	seedVerifiedSnapshot(t, db, srv.URL)
	if _, err := h.Call(t, "POST /me/unlink", plugintest.Request{UserID: "u1", Body: `{"uid":"800000000"}`}); err != nil {
		t.Fatal(err)
	}

	res, _ := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if len(res.(map[string]any)["uids"].([]string)) != 0 {
		t.Fatalf("解除されていない: %+v", res)
	}
}

func TestRoutes_RejectsBadUIDFormat(t *testing.T) {
	srv := fakeEnka(t, http.StatusOK, `{}`)
	h := setupRoutes(t, srv.URL)

	if _, err := h.Call(t, "POST /me/begin", plugintest.Request{UserID: "u1", Body: `{"uid":"abc"}`}); err == nil {
		t.Fatal("形式不正を弾いていない")
	}
}

// --- ジョブ ---

// 期限切れのものだけを取り直すこと。上流が落ちていても古いデータは消さない。
func TestJobs_RefreshKeepsStaleOnFailure(t *testing.T) {
	db := testDB(t)
	harness := plugintest.New(t).WithName("genshin").WithDB(db)

	ok := fakeEnka(t, http.StatusOK, `{"playerInfo":{"nickname":"Old","level":10},"ttl":-1}`)
	harness.WithConfig(map[string]any{"endpoint": ok.URL, "timeoutSeconds": 5}).Routes(Plugin)
	seedVerifiedSnapshot(t, db, ok.URL)

	// 期限切れにしてから、上流が落ちている状態で更新を走らせる。
	if _, err := db.Exec(`UPDATE snapshots SET expires_at = now() - interval '1 hour', fetched_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	down := fakeEnka(t, http.StatusFailedDependency, `{"message":"down"}`)
	jobs := plugintest.New(t).WithName("genshin").WithDB(db).WithAPI(defaultLinkingAPI()).
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

// **Enka の応答から現行形式の id を拾って保存すること。**
//
// `profilePicture` は `{"id": 9100}` (現行) と `{"avatarId": 10000046}` (旧) の
// 2 形式がある。現行を読まずに旧だけ見ていた頃は `0` が保存され、プロフィールの
// アイコンが一切出なかった (本番の DB で実測)。保存形式まで見るのは、
// **応答を読む所と id 空間を区別する所が別**だから — 片方だけ直しても出ない。
func TestRoutes_StoresProfilePictureID(t *testing.T) {
	for _, tt := range []struct {
		name, picture, want string
	}{
		{"現行形式", `{"id":9100}`, "pfp:9100"},
		{"旧形式", `{"avatarId":10000046}`, "10000046"},
		{"両方あれば現行形式", `{"id":9100,"avatarId":10000046}`, "pfp:9100"},
		{"どちらも無ければ空", `{}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			srv := fakeEnka(t, http.StatusOK,
				`{"playerInfo":{"nickname":"Traveler","level":60,"profilePicture":`+tt.picture+`},"ttl":300}`)
			plugintest.New(t).
				WithName("genshin").
				WithDB(db).
				WithConfig(map[string]any{"endpoint": srv.URL, "userAgent": "test/1.0", "timeoutSeconds": 5}).
				Routes(Plugin)

			seedVerifiedSnapshot(t, db, srv.URL)

			var got string
			if err := db.QueryRow(`SELECT profile_icon FROM snapshots WHERE uid = '800000000'`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("profile_icon = %q, want %q", got, tt.want)
			}
		})
	}
}

func defaultLinkingAPI() *linkingAPI { a := &linkingAPI{}; a.limit.Store(1); return a }

// Profile/job tests seed a verified registration so they can test rendering and
// refresh independently. Ownership proof is covered by linking_test.go.
func seedVerifiedSnapshot(t *testing.T, db *sql.DB, endpoint string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO accounts(user_id,uid) VALUES('u1','800000000')`); err != nil {
		t.Fatal(err)
	}
	c := newEnkaClient(settings{Endpoint: endpoint, UserAgent: "test/1.0", TimeoutSeconds: 5, Language: "ja"})
	s, err := c.fetch(context.Background(), "800000000")
	if err != nil {
		t.Fatal(err)
	}
	if err = saveSnapshot(context.Background(), db, s); err != nil {
		t.Fatal(err)
	}
}
