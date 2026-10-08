package genshin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

type refreshAPI struct {
	minutes atomic.Int64
	slow    string
	raw     string
}

type refreshCaller struct {
	api  *refreshAPI
	user string
}

func (a *refreshAPI) Anonymous() plugin.Caller { return refreshCaller{api: a} }
func (a *refreshAPI) AsUser(user string) plugin.Caller {
	return refreshCaller{api: a, user: user}
}
func (c refreshCaller) Call(_ context.Context, endpoint string, _ any) (json.RawMessage, error) {
	if endpoint != "i" {
		return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
	}
	if c.api.raw != "" {
		return json.RawMessage(c.api.raw), nil
	}
	n := c.api.minutes.Load()
	if c.api.slow != "" && c.user == c.api.slow {
		n = 60
	}
	return json.RawMessage(fmt.Sprintf(`{"host":null,"policies":{"genshinRefreshIntervalMinutes":%d}}`, n)), nil
}

func TestRefreshIntervalPolicy(t *testing.T) {
	for _, test := range []struct {
		raw     string
		minutes int
		invalid bool
	}{
		{`{"policies":{}}`, 10, false},
		{`{"policies":{"genshinRefreshIntervalMinutes":1}}`, 1, false},
		{`{"policies":{"genshinRefreshIntervalMinutes":1440}}`, 1440, false},
		{`{"policies":{"genshinRefreshIntervalMinutes":0}}`, 0, true},
		{`{"policies":{"genshinRefreshIntervalMinutes":1441}}`, 0, true},
		{`{"policies":{"genshinRefreshIntervalMinutes":1.5}}`, 0, true},
		{`{"policies":{"genshinRefreshIntervalMinutes":"10"}}`, 0, true},
		{`{"policies":{"genshinRefreshIntervalMinutes":null}}`, 0, true},
		{`{"host":"remote.example","policies":{}}`, 0, true},
		{`not-json`, 0, true},
	} {
		t.Run(test.raw, func(t *testing.T) {
			got, err := refreshInterval(context.Background(), &refreshAPI{raw: test.raw}, "u1")
			if (err != nil) != test.invalid || got != time.Duration(test.minutes)*time.Minute {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
	if _, err := refreshInterval(context.Background(), nil, "u1"); err == nil {
		t.Fatal("API未接続時に取得を許可した")
	}
}

func TestRefreshDueBoundaries(t *testing.T) {
	now := time.Now()
	fetched := sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true}
	expired := sql.NullTime{Time: now, Valid: true}
	if !refreshDue(now, fetched, expired, sql.NullTime{}, 10*time.Minute) {
		t.Fatal("間隔・TTL境界で更新しない")
	}
	if refreshDue(now.Add(-time.Nanosecond), fetched, expired, sql.NullTime{}, 10*time.Minute) {
		t.Fatal("境界より前に更新した")
	}
	if refreshDue(now, fetched, sql.NullTime{Time: now.Add(time.Hour), Valid: true}, sql.NullTime{}, time.Minute) {
		t.Fatal("Enka TTLを短縮した")
	}
	if refreshDue(now, fetched, expired, sql.NullTime{Time: now.Add(-time.Minute), Valid: true}, 10*time.Minute) {
		t.Fatal("失敗後の間隔を無視した")
	}
	if !refreshDue(now, sql.NullTime{}, sql.NullTime{}, sql.NullTime{}, 10*time.Minute) {
		t.Fatal("未取得UIDを更新しない")
	}
}

func setupRefresh(t *testing.T, api *refreshAPI, status int) (*sql.DB, *plugintest.JobSet, *atomic.Int64) {
	t.Helper()
	db := testDB(t)
	calls := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"playerInfo":{"nickname":"New"},"ttl":7200}`))
	}))
	t.Cleanup(srv.Close)
	h := plugintest.New(t).WithName("genshin").WithDB(db).WithAPI(api).
		WithConfig(map[string]any{"endpoint": srv.URL, "timeoutSeconds": 5})
	h.Routes(Plugin)
	return db, h.Jobs(Plugin), calls
}

func seedRefresh(t *testing.T, db *sql.DB, user, uid string, minutesAgo int, ttlExpired bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO accounts(user_id,uid) VALUES($1,$2)`, user, uid); err != nil {
		t.Fatal(err)
	}
	if err := saveSnapshot(context.Background(), db, &snapshot{uid: uid, nickname: "Old", ttl: 7200}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE snapshots SET fetched_at=clock_timestamp()-make_interval(mins=>$2),
		expires_at=CASE WHEN $3 THEN clock_timestamp()-interval '1 second' ELSE clock_timestamp()+interval '2 hours' END WHERE uid=$1`, uid, minutesAgo, ttlExpired); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshJobPolicyAndTTL(t *testing.T) {
	for _, test := range []struct {
		name                  string
		interval, age, expect int
		expired               bool
	}{
		{"短縮", 5, 6, 1, true},
		{"延長", 60, 15, 0, true},
		{"既定未到来", 10, 6, 0, true},
		{"TTL保持", 1, 120, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &refreshAPI{}
			api.minutes.Store(int64(test.interval))
			db, jobs, calls := setupRefresh(t, api, http.StatusOK)
			seedRefresh(t, db, "u1", "800000000", test.age, test.expired)
			if err := jobs.Run(t, "refresh", ""); err != nil {
				t.Fatal(err)
			}
			if int(calls.Load()) != test.expect {
				t.Fatalf("取得回数=%d want=%d", calls.Load(), test.expect)
			}
		})
	}
}

func TestRefreshJobPolicyChange(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(60)
	db, jobs, calls := setupRefresh(t, api, http.StatusOK)
	seedRefresh(t, db, "u1", "800000000", 7, true)
	if err := jobs.Run(t, "refresh", ""); err != nil {
		t.Fatal(err)
	}
	api.minutes.Store(5)
	if err := jobs.Run(t, "refresh", ""); err != nil || calls.Load() != 1 {
		t.Fatalf("ポリシー短縮を反映しない: calls=%d err=%v", calls.Load(), err)
	}
}

func TestRefreshJobFailureCooldown(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(10)
	db, jobs, calls := setupRefresh(t, api, http.StatusServiceUnavailable)
	seedRefresh(t, db, "u1", "800000000", 120, true)
	for range 2 {
		if err := jobs.Run(t, "refresh", ""); err != nil {
			t.Fatal(err)
		}
	}
	var nickname string
	if err := db.QueryRow(`SELECT nickname FROM snapshots WHERE uid='800000000'`).Scan(&nickname); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || nickname != "Old" {
		t.Fatalf("失敗時の古いデータ・間隔を維持しない: calls=%d nickname=%s", calls.Load(), nickname)
	}
}

func TestRefreshJobSkipsNotDueAcrossPages(t *testing.T) {
	api := &refreshAPI{slow: "slow"}
	api.minutes.Store(5)
	db, jobs, calls := setupRefresh(t, api, http.StatusOK)
	for i := range 101 {
		seedRefresh(t, db, "slow", fmt.Sprintf("800%06d", i), 15, true)
	}
	seedRefresh(t, db, "fast", "800999999", 15, true)
	if err := jobs.Run(t, "refresh", ""); err != nil || calls.Load() != 1 {
		t.Fatalf("間隔未到来の候補が後続を妨げた: calls=%d err=%v", calls.Load(), err)
	}
}

func TestRefreshJobFetchBudget(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(5)
	db, jobs, calls := setupRefresh(t, api, http.StatusOK)
	for i := range 51 {
		seedRefresh(t, db, "u1", fmt.Sprintf("800%06d", i), 15, true)
	}
	if err := jobs.Run(t, "refresh", ""); err != nil || calls.Load() != 50 {
		t.Fatalf("取得上限を守らない: calls=%d err=%v", calls.Load(), err)
	}
	if err := jobs.Run(t, "refresh", ""); err != nil || calls.Load() != 51 {
		t.Fatalf("後続UIDを更新しない: calls=%d err=%v", calls.Load(), err)
	}
}

func TestRefreshFailureDoesNotStarveLaterUID(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(1)
	db, jobs, calls := setupRefresh(t, api, http.StatusServiceUnavailable)
	for i := range 51 {
		seedRefresh(t, db, "u1", fmt.Sprintf("800%06d", i), 120, true)
	}
	if err := jobs.Run(t, "refresh", ""); err != nil || calls.Load() != 50 {
		t.Fatalf("取得枠: calls=%d err=%v", calls.Load(), err)
	}
	if _, err := db.Exec(`UPDATE accounts SET last_refresh_attempt_at=clock_timestamp()-interval '2 minutes' WHERE last_refresh_attempt_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(t, "refresh", ""); err != nil {
		t.Fatal(err)
	}
	var attempted sql.NullTime
	if err := db.QueryRow(`SELECT last_refresh_attempt_at FROM accounts WHERE uid='800000050'`).Scan(&attempted); err != nil || !attempted.Valid {
		t.Fatalf("先頭50件の失敗が後続を妨げた: err=%v", err)
	}
}

func TestRefreshSnapshotWriteFailurePreservesCooldown(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(10)
	db, jobs, calls := setupRefresh(t, api, http.StatusOK)
	seedRefresh(t, db, "u1", "800000000", 120, true)
	if _, err := db.Exec(`ALTER TABLE snapshots ADD CONSTRAINT reject_new_snapshot CHECK (nickname <> 'New')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := jobs.Run(t, "refresh", ""); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("保存失敗後に間隔を無視した: %d", calls.Load())
	}
}

func TestRefreshUIDSerializesConcurrentJobs(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(5)
	db := testDB(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"playerInfo":{"nickname":"New"},"ttl":7200}`))
	}))
	defer srv.Close()
	plugintest.New(t).WithName("genshin").WithDB(db).WithAPI(api).Routes(Plugin)
	seedRefresh(t, db, "u1", "800000000", 15, true)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := refreshUID(context.Background(), api, db, testClient(srv.URL), "800000000")
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("同じUIDを重複取得した: %d", calls.Load())
	}
}

func TestRefreshUIDRechecksPolicyUnderLock(t *testing.T) {
	api := &refreshAPI{}
	api.minutes.Store(5)
	db, _, _ := setupRefresh(t, api, http.StatusOK)
	seedRefresh(t, db, "u1", "800000000", 15, true)
	lock, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('800000000',48128))`); err != nil {
		t.Fatal(err)
	}
	api.minutes.Store(60)
	result := make(chan error, 1)
	go func() {
		attempted, err := refreshUID(context.Background(), api, db, testClient("http://invalid.invalid"), "800000000")
		if attempted && err == nil {
			err = fmt.Errorf("延長後のポリシーを無視した")
		}
		result <- err
	}()
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshMigrationPreservesExistingAccounts(t *testing.T) {
	db := testDB(t)
	ordered := append([]plugin.Migration(nil), Plugin.Migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })
	for _, migration := range ordered {
		if migration.Version >= 10 {
			continue
		}
		if _, err := db.Exec(migration.SQL); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO accounts(user_id,uid) VALUES('u1','800000000')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(refreshMigration.SQL); err != nil {
		t.Fatal(err)
	}
	var user, uid string
	var attemptedAt sql.NullTime
	if err := db.QueryRow(`SELECT user_id,uid,last_refresh_attempt_at FROM accounts`).Scan(&user, &uid, &attemptedAt); err != nil {
		t.Fatal(err)
	}
	if user != "u1" || uid != "800000000" || attemptedAt.Valid {
		t.Fatal("旧連携データを変更した")
	}
}
