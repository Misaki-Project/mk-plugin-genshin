/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

package genshin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// buildProfile assembles the display payload for a local user.
//
// 未登録なら (nil, nil)。**エラーと区別する** — 「登録していない」は普通の
// 状態で、表示側はそれを見て何も描かない。
func buildProfile(c context.Context, db *sql.DB, client *enkaClient, userID string) (map[string]any, error) {
	var (
		uid, nickname, signature, region, profileIcon string
		level, worldLevel, nameCardID                 int
		achievements, towerFloor, towerLevel          int
		towerStar, theaterAct, theaterMode            int
		theaterStar, fetterCount                      int
		showcaseRaw, charactersRaw                    []byte
		fetchedAt                                     time.Time
	)
	err := db.QueryRowContext(c, `
		SELECT a.uid, s.nickname, s.level, s.world_level, s.signature, s.fetched_at,
		       s.name_card_id, s.region, s.achievements, s.tower_floor, s.tower_level,
		       s.profile_icon, s.showcase,
		       s.tower_star, s.theater_act, s.theater_mode, s.theater_star,
		       s.fetter_count, s.characters
		FROM accounts a JOIN snapshots s ON s.uid = a.uid
		WHERE a.user_id = $1
	`, userID).Scan(&uid, &nickname, &level, &worldLevel, &signature, &fetchedAt,
		&nameCardID, &region, &achievements, &towerFloor, &towerLevel, &profileIcon, &showcaseRaw,
		&towerStar, &theaterAct, &theaterMode, &theaterStar, &fetterCount, &charactersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
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
		if info, ok := client.chars.Lookup(c, id); ok {
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
		"nameCard":      nameCardURL(c, client.namecards, nameCardID),
		"showcase":      cards,
		"fetchedAt":     fetchedAt,
	}, nil
}
