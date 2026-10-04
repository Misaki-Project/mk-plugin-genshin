package genshin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/shiroha-a/mk/plugin"
)

const refreshIntervalPolicy = "genshinRefreshIntervalMinutes"
const defaultRefreshInterval = 10 * time.Minute
const maxRefreshAttempts = 50
const refreshPageSize = 100

// 既存migrationは変更せず、自動更新の失敗時にも取得間隔を維持する。
var refreshMigration = plugin.Migration{Version: 10, SQL: `
	ALTER TABLE accounts ADD COLUMN last_refresh_attempt_at timestamptz;
`}

func refreshInterval(c context.Context, api plugin.API, userID string) (time.Duration, error) {
	if api == nil {
		return 0, plugin.Errorf(503, "原神の取得間隔を確認できません")
	}
	raw, err := api.AsUser(userID).Call(c, "i", map[string]any{})
	if err != nil {
		return 0, err
	}
	var me struct {
		Host     *string                    `json:"host"`
		Policies map[string]json.RawMessage `json:"policies"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return 0, err
	}
	if me.Host != nil {
		return 0, plugin.Errorf(403, "ローカルアカウントが必要です")
	}
	value, ok := me.Policies[refreshIntervalPolicy]
	if !ok {
		return defaultRefreshInterval, nil
	}
	var minutes float64
	if json.Unmarshal(value, &minutes) != nil || minutes < 1 || minutes > 1440 || math.Trunc(minutes) != minutes {
		return 0, plugin.Errorf(503, "原神の取得間隔は1〜1440分の整数で指定してください")
	}
	return time.Duration(minutes) * time.Minute, nil
}

func refreshDue(now time.Time, fetchedAt, expiresAt, attemptedAt sql.NullTime, interval time.Duration) bool {
	if expiresAt.Valid && now.Before(expiresAt.Time) {
		return false
	}
	last := fetchedAt
	if attemptedAt.Valid && (!last.Valid || attemptedAt.Time.After(last.Time)) {
		last = attemptedAt
	}
	return !last.Valid || !now.Before(last.Time.Add(interval))
}

type refreshCandidate struct {
	uid, userID                       string
	fetchedAt, expiresAt, attemptedAt sql.NullTime
	now                               time.Time
}

// TTL切れの候補を取得・試行時刻の古い順にkeysetで走査する。間隔未到来のUIDは
// 50件の取得枠を消費せず、取得失敗したUIDも次回は後ろへ回す。
func refreshExpired(c context.Context, ctx plugin.Context, db *sql.DB, client *enkaClient) error {
	type policyResult struct {
		interval time.Duration
		err      error
	}
	policies := map[string]policyResult{}
	var startedAt time.Time
	if err := db.QueryRowContext(c, `SELECT clock_timestamp()`).Scan(&startedAt); err != nil {
		return err
	}
	cursor, attempts := "", 0
	var cursorTime any = "-infinity"
	for attempts < maxRefreshAttempts {
		rows, err := db.QueryContext(c, `SELECT a.uid,a.user_id,s.fetched_at,s.expires_at,
			a.last_refresh_attempt_at,clock_timestamp() FROM accounts a
			LEFT JOIN snapshots s ON s.uid=a.uid
			WHERE (COALESCE(GREATEST(a.last_refresh_attempt_at,s.fetched_at),'-infinity'::timestamptz),a.uid)>($1::timestamptz,$2)
			AND (s.uid IS NULL OR s.expires_at<=clock_timestamp())
			AND (a.last_refresh_attempt_at IS NULL OR a.last_refresh_attempt_at<=$4)
			ORDER BY COALESCE(GREATEST(a.last_refresh_attempt_at,s.fetched_at),'-infinity'::timestamptz),a.uid LIMIT $3`, cursorTime, cursor, refreshPageSize, startedAt)
		if err != nil {
			return err
		}
		page := []refreshCandidate{}
		for rows.Next() {
			var item refreshCandidate
			if err := rows.Scan(&item.uid, &item.userID, &item.fetchedAt, &item.expiresAt, &item.attemptedAt, &item.now); err != nil {
				_ = rows.Close()
				return err
			}
			page = append(page, item)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			cursor = item.uid
			cursorTime = "-infinity"
			if item.fetchedAt.Valid {
				cursorTime = item.fetchedAt.Time
			}
			if item.attemptedAt.Valid && (!item.fetchedAt.Valid || item.attemptedAt.Time.After(item.fetchedAt.Time)) {
				cursorTime = item.attemptedAt.Time
			}
			policy, ok := policies[item.userID]
			if !ok {
				policy.interval, policy.err = refreshInterval(c, ctx.API(), item.userID)
				policies[item.userID] = policy
				if policy.err != nil {
					ctx.Logger().Warn("原神の取得間隔を確認できません", "userId", item.userID, "err", policy.err)
				}
			}
			if policy.err != nil || !refreshDue(item.now, item.fetchedAt, item.expiresAt, item.attemptedAt, policy.interval) {
				continue
			}
			attempted, err := refreshUID(c, ctx.API(), db, client, item.uid)
			if attempted {
				attempts++
			}
			if err != nil {
				ctx.Logger().Warn("更新に失敗しました", "uid", item.uid, "err", err)
			}
			if attempts == maxRefreshAttempts {
				return nil
			}
		}
	}
	return nil
}

// 本人確認とUIDロックを共有し、ロック取得後に所有者・TTL・最新ポリシーを
// 再確認する。失敗した取得も時刻を保存し、古いスナップショットは残す。
func refreshUID(c context.Context, api plugin.API, db *sql.DB, client *enkaClient, uid string) (bool, error) {
	tx, err := db.BeginTx(c, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(c, `SELECT pg_advisory_xact_lock(hashtextextended($1, 48128))`, uid); err != nil {
		return false, err
	}
	var item refreshCandidate
	err = tx.QueryRowContext(c, `SELECT a.user_id,s.fetched_at,s.expires_at,a.last_refresh_attempt_at,
		clock_timestamp() FROM accounts a LEFT JOIN snapshots s ON s.uid=a.uid
		WHERE a.uid=$1 FOR UPDATE OF a`, uid).
		Scan(&item.userID, &item.fetchedAt, &item.expiresAt, &item.attemptedAt, &item.now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	interval, err := refreshInterval(c, api, item.userID)
	if err != nil || !refreshDue(item.now, item.fetchedAt, item.expiresAt, item.attemptedAt, interval) {
		return false, err
	}
	if _, err := tx.ExecContext(c, `UPDATE accounts SET last_refresh_attempt_at=clock_timestamp() WHERE uid=$1`, uid); err != nil {
		return false, err
	}
	snap, fetchErr := client.fetch(c, uid)
	if fetchErr == nil {
		if _, err := tx.ExecContext(c, `SAVEPOINT snapshot_save`); err != nil {
			return true, err
		}
		if err := saveSnapshot(c, tx, snap); err != nil {
			if _, rollbackErr := tx.ExecContext(c, `ROLLBACK TO SAVEPOINT snapshot_save`); rollbackErr != nil {
				return true, rollbackErr
			}
			fetchErr = err
		}
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	return true, fetchErr
}
