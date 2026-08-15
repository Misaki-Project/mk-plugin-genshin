// Package genshin shows a user's Genshin Impact profile (nickname / Adventure
// Rank) on their Misskey profile.
//
// データ元は Enka.Network (https://enka.network/)。認証不要の公開 API だが、
// ttl に従ったキャッシュを求められているのでそれに従う。
package genshin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/shiroha-a/mk/plugin"
)

// Plugin is the entry point referenced by the generated registration code.
var Plugin = plugin.Definition{
	Name:       "genshin",
	Version:    "0.1.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
}

// settings mirrors the `plugins.genshin` section of the instance config.
type settings struct {
	// Endpoint is the Enka.Network base URL. テストで差し替えられるように
	// 設定にしている。
	Endpoint string `json:"endpoint"`
	// UserAgent identifies this instance to Enka.Network. 向こうが
	// 「追跡できるように付けてほしい」と明示しているので既定でも名乗る。
	UserAgent string `json:"userAgent"`
	// TimeoutSeconds bounds one upstream request.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// Language selects the slice of Enka's localisation files to use for
	// character / weapon / artifact names.
	Language string `json:"language"`
}

func loadSettings(ctx plugin.Context) (settings, error) {
	s := settings{
		Endpoint:       "https://enka.network",
		UserAgent:      "mk-go-plugin-genshin/0.1 (+https://github.com/shiroha-a/mk)",
		TimeoutSeconds: 10,
		Language:       "ja",
	}
	if err := ctx.Config().Unmarshal(&s); err != nil {
		return s, err
	}
	return s, nil
}

var migrations = []plugin.Migration{
	{Version: 3, SQL: `
		ALTER TABLE snapshots
			ADD COLUMN tower_star   int   NOT NULL DEFAULT 0,
			ADD COLUMN theater_act  int   NOT NULL DEFAULT 0,
			ADD COLUMN theater_mode int   NOT NULL DEFAULT 0,
			ADD COLUMN theater_star int   NOT NULL DEFAULT 0,
			ADD COLUMN fetter_count int   NOT NULL DEFAULT 0,
			ADD COLUMN characters   jsonb NOT NULL DEFAULT '[]'
	`},
	{Version: 2, SQL: `
		ALTER TABLE snapshots
			ADD COLUMN name_card_id  int  NOT NULL DEFAULT 0,
			ADD COLUMN region        text NOT NULL DEFAULT '',
			ADD COLUMN achievements  int  NOT NULL DEFAULT 0,
			ADD COLUMN tower_floor   int  NOT NULL DEFAULT 0,
			ADD COLUMN tower_level   int  NOT NULL DEFAULT 0,
			ADD COLUMN profile_icon  text NOT NULL DEFAULT '',
			ADD COLUMN showcase      jsonb NOT NULL DEFAULT '[]'
	`},
	{Version: 1, SQL: `
		CREATE TABLE accounts (
			user_id    text PRIMARY KEY,
			uid        text NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE snapshots (
			uid         text PRIMARY KEY,
			nickname    text NOT NULL,
			level       int  NOT NULL,
			world_level int  NOT NULL,
			signature   text NOT NULL,
			fetched_at  timestamptz NOT NULL DEFAULT now(),
			expires_at  timestamptz NOT NULL
		);
	`},
}

// uidPattern matches a Genshin UID. 9 桁が基本だが、サーバーによって 10 桁も
// あるので幅を持たせる。形式が違うものは upstream に投げる前に弾く
// (向こうのレート制限を無駄に消費しない)。
var uidPattern = regexp.MustCompile(`^[1-9][0-9]{8,9}$`)

func routes(ctx plugin.Context, r plugin.Router) error {
	set, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()
	client := newEnkaClient(set)

	// frontend から呼ぶものは POST にする。misskeyApi (= host.api) が POST
	// 固定で、Misskey 本体の API も POST 基本なのでそれに倣う。

	r.POST("/me", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		var uid string
		var updated *time.Time
		err := db.QueryRowContext(req.Context(),
			`SELECT uid, updated_at FROM accounts WHERE user_id = $1`, me).Scan(&uid, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{"uid": nil}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"uid": uid, "updatedAt": updated}, nil
	})

	r.POST("/me/set", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		var body struct {
			UID string `json:"uid"`
		}
		if err := req.Bind(&body); err != nil {
			return nil, plugin.Errorf(http.StatusBadRequest, "リクエストを読めません")
		}

		// 空文字は登録解除として扱う。UI から消したときに消せないと不便。
		if body.UID == "" {
			if _, err := db.ExecContext(req.Context(), `DELETE FROM accounts WHERE user_id = $1`, me); err != nil {
				return nil, err
			}
			return map[string]any{"uid": nil}, nil
		}
		if !uidPattern.MatchString(body.UID) {
			return nil, plugin.Errorf(http.StatusBadRequest, "UID の形式が正しくありません")
		}

		// **登録時に 1 度だけ取得して存在を確かめる。** 存在しない UID を黙って
		// 保存すると、プロフィールに何も出ない理由が利用者に分からない。
		snap, err := client.fetch(req.Context(), body.UID)
		if err != nil {
			var ue *upstreamError
			if errors.As(err, &ue) && ue.userFacing != "" {
				return nil, plugin.Errorf(ue.status, "%s", ue.userFacing)
			}
			// 上流の一時的な不調で登録を拒むと、直るまで設定できない。
			// 保存だけして、表示は次の更新に任せる。
			ctx.Logger().Warn("登録時の取得に失敗しました (保存は行います)", "err", err)
		} else if err := saveSnapshot(req.Context(), db, snap); err != nil {
			return nil, err
		}

		if _, err := db.ExecContext(req.Context(), `
			INSERT INTO accounts (user_id, uid, updated_at) VALUES ($1, $2, now())
			ON CONFLICT (user_id) DO UPDATE SET uid = EXCLUDED.uid, updated_at = now()
		`, me, body.UID); err != nil {
			return nil, err
		}
		return map[string]any{"uid": body.UID}, nil
	})

	r.POST("/profile", func(req plugin.Request) (any, error) {
		var body struct {
			UserID string `json:"userId"`
		}
		if err := req.Bind(&body); err != nil || body.UserID == "" {
			return nil, plugin.Errorf(http.StatusBadRequest, "userId が必要です")
		}

		var (
			uid, nickname, signature, region, profileIcon string
			level, worldLevel, nameCardID                 int
			achievements, towerFloor, towerLevel          int
			towerStar, theaterAct, theaterMode            int
			theaterStar, fetterCount                      int
			showcaseRaw, charactersRaw                    []byte
			fetchedAt                                     time.Time
		)
		err := db.QueryRowContext(req.Context(), `
			SELECT a.uid, s.nickname, s.level, s.world_level, s.signature, s.fetched_at,
			       s.name_card_id, s.region, s.achievements, s.tower_floor, s.tower_level,
			       s.profile_icon, s.showcase,
			       s.tower_star, s.theater_act, s.theater_mode, s.theater_star,
			       s.fetter_count, s.characters
			FROM accounts a JOIN snapshots s ON s.uid = a.uid
			WHERE a.user_id = $1
		`, body.UserID).Scan(&uid, &nickname, &level, &worldLevel, &signature, &fetchedAt,
			&nameCardID, &region, &achievements, &towerFloor, &towerLevel, &profileIcon, &showcaseRaw,
			&towerStar, &theaterAct, &theaterMode, &theaterStar, &fetterCount, &charactersRaw)
		if errors.Is(err, sql.ErrNoRows) {
			// 未登録は「無い」であってエラーではない。プロフィール表示側は
			// これを見て何も描かない。
			return map[string]any{"linked": false}, nil
		}
		if err != nil {
			return nil, err
		}

		var showcase []showcaseEntry
		if len(showcaseRaw) > 0 {
			_ = json.Unmarshal(showcaseRaw, &showcase)
		}
		// 壊れた JSON でカード全体を落とさない。詳細が出ないだけで済ませる。
		characters := []character{}
		if len(charactersRaw) > 0 {
			_ = json.Unmarshal(charactersRaw, &characters)
		}

		// アイコンは**自分のプロキシ経由の URL**として返す。CSP が
		// `img-src 'self'` なので、取得元の URL を渡しても表示できない。
		profileIconURL := ""
		if id, convErr := strconv.Atoi(profileIcon); convErr == nil && id != 0 {
			if info, ok := client.chars.Lookup(req.Context(), id); ok {
				profileIconURL = assetURL(info.IconName())
			}
		}
		cards := make([]map[string]any, 0, len(showcase))
		for _, e := range showcase {
			cards = append(cards, map[string]any{
				"level": e.Level, "element": e.Element, "icon": assetURL(e.Icon),
			})
		}

		return map[string]any{
			"linked":        true,
			"uid":           uid,
			"nickname":      nickname,
			"adventureRank": level,
			"worldLevel":    worldLevel,
			"signature":     signature,
			"region":        region,
			"achievements":  achievements,
			"spiral":        spiralLabel(towerFloor, towerLevel),
			"spiralStars":   towerStar,
			"theater":       theaterLabel(theaterAct, theaterMode),
			"theaterStars":  theaterStar,
			"fetterCount":   fetterCount,
			"characters":    characters,
			"profileIcon":   profileIconURL,
			"nameCard":      nameCardURL(req.Context(), client.namecards, nameCardID),
			"showcase":      cards,
			"fetchedAt":     fetchedAt,
		}, nil
	})

	// 画像プロキシ。本体の CSP は `img-src 'self'` なので、外部の画像を
	// <img> で直接読めない。同一オリジンで配信することで CSP を緩めずに済む。
	//
	// **GET にする。** ブラウザの <img> は GET しか出さない。
	r.GET("/asset/:name", func(req plugin.Request) (any, error) {
		body, ct, err := fetchAsset(req.Context(), client.http, set.UserAgent, req.Param("name"))
		if err != nil {
			var ue *upstreamError
			if errors.As(err, &ue) && ue.status == http.StatusBadRequest {
				return nil, plugin.Errorf(http.StatusBadRequest, "asset 名が不正です")
			}
			return nil, plugin.ErrNotFound("asset が見つかりません")
		}
		return plugin.Blob{
			ContentType: ct,
			Body:        body,
			// 静的アセットなので長めに持たせる。取得元の負荷も減る。
			CacheControl: "public, max-age=86400, immutable",
		}, nil
	})

	return nil
}

func jobs(ctx plugin.Context, j plugin.Jobs) error {
	set, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()
	client := newEnkaClient(set)

	j.Handle("refresh", func(c context.Context, _ json.RawMessage) error {
		return refreshExpired(c, ctx, db, client)
	})
	j.Schedule("*/10 * * * *", "refresh", nil)
	return nil
}

// refreshExpired re-fetches snapshots whose ttl has run out.
//
// **上流が落ちていても古いデータは消さない。** 原神のデータが取れないせいで
// プロフィール表示が空になる方が困る (実際 Enka は upstream 不調で 424 を返す
// ことがある)。
func refreshExpired(c context.Context, ctx plugin.Context, db *sql.DB, client *enkaClient) error {
	rows, err := db.QueryContext(c, `
		SELECT DISTINCT a.uid FROM accounts a
		LEFT JOIN snapshots s ON s.uid = a.uid
		WHERE s.uid IS NULL OR s.expires_at <= now()
		LIMIT 50
	`)
	if err != nil {
		return err
	}
	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			_ = rows.Close()
			return err
		}
		uids = append(uids, uid)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, uid := range uids {
		snap, err := client.fetch(c, uid)
		if err != nil {
			// 1 件の失敗で全体を止めない。次回の実行で再試行される。
			ctx.Logger().Warn("取得に失敗しました", "uid", uid, "err", err)
			continue
		}
		if err := saveSnapshot(c, db, snap); err != nil {
			ctx.Logger().Warn("保存に失敗しました", "uid", uid, "err", err)
		}
	}
	return nil
}

func saveSnapshot(c context.Context, db *sql.DB, s *snapshot) error {
	showcase, err := json.Marshal(s.showcase)
	if err != nil {
		return err
	}
	if s.showcase == nil {
		showcase = []byte("[]")
	}
	characters, err := json.Marshal(s.characters)
	if err != nil {
		return err
	}
	if s.characters == nil {
		characters = []byte("[]")
	}
	_, err = db.ExecContext(c, `
		INSERT INTO snapshots (
			uid, nickname, level, world_level, signature, fetched_at, expires_at,
			name_card_id, region, achievements, tower_floor, tower_level, profile_icon, showcase,
			tower_star, theater_act, theater_mode, theater_star, fetter_count, characters)
		VALUES ($1, $2, $3, $4, $5, now(), now() + make_interval(secs => $6),
			$7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (uid) DO UPDATE SET
			nickname = EXCLUDED.nickname, level = EXCLUDED.level,
			world_level = EXCLUDED.world_level, signature = EXCLUDED.signature,
			fetched_at = EXCLUDED.fetched_at, expires_at = EXCLUDED.expires_at,
			name_card_id = EXCLUDED.name_card_id, region = EXCLUDED.region,
			achievements = EXCLUDED.achievements, tower_floor = EXCLUDED.tower_floor,
			tower_level = EXCLUDED.tower_level, profile_icon = EXCLUDED.profile_icon,
			showcase = EXCLUDED.showcase,
			tower_star = EXCLUDED.tower_star, theater_act = EXCLUDED.theater_act,
			theater_mode = EXCLUDED.theater_mode, theater_star = EXCLUDED.theater_star,
			fetter_count = EXCLUDED.fetter_count, characters = EXCLUDED.characters
	`, s.uid, s.nickname, s.level, s.worldLevel, s.signature, s.ttl,
		s.nameCardID, s.region, s.achievements, s.towerFloor, s.towerLevel,
		strconv.Itoa(s.profileIcon), showcase,
		s.towerStar, s.theaterAct, s.theaterMode, s.theaterStar, s.fetterCount, characters)
	return err
}

// --- Enka.Network ---

type snapshot struct {
	uid          string
	nickname     string
	level        int
	worldLevel   int
	signature    string
	nameCardID   int
	region       string
	achievements int
	towerFloor   int
	towerLevel   int
	towerStar    int
	theaterAct   int
	theaterMode  int
	theaterStar  int
	fetterCount  int
	profileIcon  int
	showcase     []showcaseEntry
	// characters holds the full build of each showcased character.
	characters []character
	ttl        int
}

// showcaseEntry is one character in the player's showcase.
type showcaseEntry struct {
	AvatarID int    `json:"avatarId"`
	Level    int    `json:"level"`
	Icon     string `json:"icon"`
	Element  string `json:"element"`
}

type upstreamError struct {
	status int
	// userFacing is non-empty when the failure is the user's fault and should
	// be shown to them (invalid UID / no such player).
	userFacing string
	msg        string
}

func (e *upstreamError) Error() string { return e.msg }

type enkaClient struct {
	set       settings
	http      *http.Client
	chars     *characterStore
	namecards *characterStore
	// texts resolves name hashes (キャラ / 武器 / 聖遺物セット)。
	texts *textStore
	// uiTexts resolves UI keys like FIGHT_PROP_CRITICAL.
	uiTexts *textStore
	// relicSets covers artifact sets whose name hash is missing from loc.json.
	relicSets *relicSetStore
}

// newEnkaClient wires the client and its master-data stores.
func newEnkaClient(set settings) *enkaClient {
	hc := &http.Client{Timeout: time.Duration(set.TimeoutSeconds) * time.Second}
	return &enkaClient{
		set: set, http: hc,
		chars:     newCharacterStore(hc),
		namecards: newNamecardStore(hc),
		texts:     newTextStore(hc, locURL, set.Language),
		uiTexts:   newTextStore(hc, uiLocURL, set.Language),
		relicSets: newRelicSetStore(hc),
	}
}

// fetch retrieves the full profile for a UID.
//
// `?info` を付けると playerInfo だけになって軽いが、ショーケースのビルド
// (avatarInfoList) が落ちる。表示する以上は取る。**ttl を必ず守る**ことで
// レート制限に配慮する (取得元が明示している要求事項)。
func (c *enkaClient) fetch(ctx context.Context, uid string) (*snapshot, error) {
	url := c.set.Endpoint + "/api/uid/" + uid
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.set.UserAgent)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enka への接続に失敗しました: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return nil, &upstreamError{status: http.StatusBadRequest,
			userFacing: "UID の形式が正しくありません", msg: "enka: 400"}
	case http.StatusNotFound:
		return nil, &upstreamError{status: http.StatusNotFound,
			userFacing: "その UID のプレイヤーが見つかりません", msg: "enka: 404"}
	default:
		// 429 (レート制限) / 424 (ゲーム側に届かない) / 5xx。いずれも
		// こちらの都合ではないので、利用者には見せずキャッシュで凌ぐ。
		return nil, &upstreamError{status: res.StatusCode,
			msg: fmt.Sprintf("enka: status %d", res.StatusCode)}
	}

	// avatarInfoList を含めると 1 件で数百 KB になる。上限は残しつつ広げる。
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		PlayerInfo struct {
			Nickname             string `json:"nickname"`
			Level                int    `json:"level"`
			WorldLevel           int    `json:"worldLevel"`
			Signature            string `json:"signature"`
			NameCardID           int    `json:"nameCardId"`
			FinishAchievementNum int    `json:"finishAchievementNum"`
			TowerFloorIndex      int    `json:"towerFloorIndex"`
			TowerLevelIndex      int    `json:"towerLevelIndex"`
			TowerStarIndex       int    `json:"towerStarIndex"`
			TheaterActIndex      int    `json:"theaterActIndex"`
			TheaterModeIndex     int    `json:"theaterModeIndex"`
			TheaterStarIndex     int    `json:"theaterStarIndex"`
			FetterCount          int    `json:"fetterCount"`
			ProfilePicture       struct {
				AvatarID int `json:"avatarId"`
			} `json:"profilePicture"`
			ShowAvatarInfoList []struct {
				AvatarID int `json:"avatarId"`
				Level    int `json:"level"`
			} `json:"showAvatarInfoList"`
		} `json:"playerInfo"`
		AvatarInfoList []rawAvatar `json:"avatarInfoList"`
		Region         string      `json:"region"`
		TTL            int         `json:"ttl"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("enka の応答を解釈できません: %w", err)
	}
	ttl := parsed.TTL
	if ttl <= 0 {
		// ttl が無い応答でも、間を置かずに再取得しない。
		ttl = 300
	}
	pi := parsed.PlayerInfo
	snap := &snapshot{
		uid: uid, nickname: pi.Nickname, level: pi.Level, worldLevel: pi.WorldLevel,
		signature: pi.Signature, nameCardID: pi.NameCardID, region: parsed.Region,
		achievements: pi.FinishAchievementNum,
		towerFloor:   pi.TowerFloorIndex, towerLevel: pi.TowerLevelIndex,
		towerStar:   pi.TowerStarIndex,
		theaterAct:  pi.TheaterActIndex,
		theaterMode: pi.TheaterModeIndex,
		theaterStar: pi.TheaterStarIndex,
		fetterCount: pi.FetterCount,
		profileIcon: pi.ProfilePicture.AvatarID, ttl: ttl,
		characters: make([]character, 0, len(parsed.AvatarInfoList)),
	}

	// ショーケースは最大 8 体。取得元が想定外の数を返しても保存が膨らまない
	// ように、showcase と同じくここでも切る。
	for i, a := range parsed.AvatarInfoList {
		if i >= 8 {
			break
		}
		snap.characters = append(snap.characters, c.buildCharacter(ctx, a))
	}

	// **ショーケースのキャラは 8 件までに切る。** 表示に使うのは数件で、
	// 取得元が想定外の数を返したときに保存が膨らむのを防ぐ。
	for i, a := range pi.ShowAvatarInfoList {
		if i >= 8 {
			break
		}
		e := showcaseEntry{AvatarID: a.AvatarID, Level: a.Level}
		if info, ok := c.chars.Lookup(ctx, a.AvatarID); ok {
			e.Icon = info.IconName()
			e.Element = info.Element
		}
		snap.showcase = append(snap.showcase, e)
	}
	return snap, nil
}
