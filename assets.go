package genshin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
 * Enka の静的アセット (キャラアイコン / 名刺画像) を扱う。
 *
 * # なぜプロキシするのか
 *
 * mk-go の CSP は `img-src 'self' data: blob:` なので、enka.network の画像を
 * <img> で直接読めない。CSP を緩めるのは本体全体に影響するので、プラグインが
 * 同一オリジンで配信する。取得元にも優しい (こちらでキャッシュできる)。
 */

// assetBase is where Enka serves UI images.
const assetBase = "https://enka.network/ui/"

// Enka publishes these id -> icon-name maps as static files.
const (
	charactersURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/characters.json"
	namecardsURL  = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/namecards.json"
	// locURL maps text hashes to display names (キャラ名・武器名・聖遺物セット名)。
	locURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/loc.json"
	// uiLocURL maps UI keys like `FIGHT_PROP_CRITICAL` to labels.
	// **ステータス名を自前で持たない**ためにこれを使う (取り違えが起きない)。
	uiLocURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/gi/locs.json"
	// relicsURL carries the artifact set masters. 新しいセットは loc.json 側の
	// 更新が追いつかず setNameTextMapHash を引けないので、こちらから補う。
	relicsURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/gi/relics.json"
)

// maxAssetBytes bounds one proxied image.
//
// **上限は必ず設ける。** 取得元が壊れた応答を返したときに、こちらのメモリを
// 際限なく使わせない。実物は 100KB 前後なので 4MB あれば十分。
const maxAssetBytes = 4 << 20

// maxLocBytes bounds the localisation files. loc.json は全言語で 350KB 程度。
const maxLocBytes = 8 << 20

// assetNamePattern restricts what can be proxied.
//
// **リクエストの文字列がそのまま取得先 URL になる。** `../` や別ホストへ
// 逃げられないよう、Enka の命名規則に合う文字だけを通す。
var assetNamePattern = func() func(string) bool {
	return func(s string) bool {
		if s == "" || len(s) > 128 {
			return false
		}
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '_' || r == '-':
			default:
				return false
			}
		}
		return true
	}
}()

// characterInfo is the subset of Enka's masters we need.
//
// characters.json と namecards.json でキー名が違う (SideIconName / icon) ので、
// 両方を受けて 1 つの型に寄せる。
type characterInfo struct {
	SideIconName string `json:"SideIconName"`
	Element      string `json:"Element"`
	// Icon is namecards.json's field.
	Icon string `json:"icon"`
	// NameTextMapHash resolves to the character name through loc.json.
	NameTextMapHash int64 `json:"NameTextMapHash"`
	// Consts holds the constellation icons in unlock order.
	Consts []string `json:"Consts"`
	// SkillOrder lists the talent ids in display order (通常 / スキル / 爆発)。
	SkillOrder []int `json:"SkillOrder"`
	// Skills maps a talent id to its icon name.
	Skills map[string]string `json:"Skills"`
	// ProudMap maps a talent id to the proud-skill group id used by
	// `proudSkillExtraLevelMap` (命ノ星座による天賦強化の加算先)。
	ProudMap map[string]int `json:"ProudMap"`
}

// IconName derives the front-facing icon from the side icon.
//
// Enka は `UI_AvatarIcon_Side_Ambor` (横顔) しか持たないが、正面の
// `UI_AvatarIcon_Ambor` も同じ命名規則で配信されている。
func (c characterInfo) IconName() string {
	if c.SideIconName == "" {
		return c.Icon
	}
	return strings.Replace(c.SideIconName, "_Side_", "_", 1)
}

// characterStore caches the avatarId -> characterInfo mapping.
//
// 134 件の静的データで滅多に変わらないので、プロセス内に持って日次で取り直す。
type characterStore struct {
	mu       sync.RWMutex
	byID     map[string]characterInfo
	fetched  time.Time
	client   *http.Client
	endpoint string
}

func newCharacterStore(client *http.Client) *characterStore {
	return &characterStore{client: client, endpoint: charactersURL}
}

// newNamecardStore reads the namecard id -> picture mapping.
func newNamecardStore(client *http.Client) *characterStore {
	return &characterStore{client: client, endpoint: namecardsURL}
}

// Lookup returns the character info for an avatarId.
//
// **nil レシーバでも落とさない。** master を配線し忘れても、名前やアイコンが
// 出ないだけでカードは成立させる。
func (s *characterStore) Lookup(ctx context.Context, avatarID int) (characterInfo, bool) {
	if s == nil {
		return characterInfo{}, false
	}
	s.mu.RLock()
	fresh := time.Since(s.fetched) < 24*time.Hour && s.byID != nil
	info, ok := s.byID[strconv.Itoa(avatarID)]
	s.mu.RUnlock()

	if fresh {
		return info, ok
	}
	if err := s.refresh(ctx); err != nil {
		// 取り直せなくても、古いものが手元にあればそれを使う。キャラ名が
		// 出ないだけでカード自体は表示できる方がよい。
		s.mu.RLock()
		defer s.mu.RUnlock()
		info, ok := s.byID[strconv.Itoa(avatarID)]
		return info, ok
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok = s.byID[strconv.Itoa(avatarID)]
	return info, ok
}

func (s *characterStore) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("characters.json: status %d", res.StatusCode)
	}

	var parsed map[string]characterInfo
	if err := json.NewDecoder(io.LimitReader(res.Body, maxAssetBytes)).Decode(&parsed); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID = parsed
	s.fetched = time.Now()
	return nil
}

// textStore caches one language slice of Enka's localisation files.
//
// loc.json は全 17 言語で 350KB あるが、使うのは 1 言語だけなので取り込み時に
// 絞る (日本語なら 22KB)。キャラ名・武器名・聖遺物セット名・ステータス名は
// すべてこれで解決するので、**表示文字列を自前で持たなくて済む**。
type textStore struct {
	mu       sync.RWMutex
	byKey    map[string]string
	fetched  time.Time
	client   *http.Client
	endpoint string
	lang     string
}

func newTextStore(client *http.Client, endpoint, lang string) *textStore {
	return &textStore{client: client, endpoint: endpoint, lang: lang}
}

// Lookup resolves a key (text hash or UI key) to its display string.
func (s *textStore) Lookup(ctx context.Context, key string) (string, bool) {
	if s == nil || key == "" || key == "0" {
		return "", false
	}
	s.mu.RLock()
	fresh := time.Since(s.fetched) < 24*time.Hour && s.byKey != nil
	v, ok := s.byKey[key]
	s.mu.RUnlock()
	if fresh {
		return v, ok
	}
	if err := s.refresh(ctx); err != nil {
		// 取り直せなくても手元のもので凌ぐ。名前が出ないだけで表示は成立する。
		s.mu.RLock()
		defer s.mu.RUnlock()
		v, ok := s.byKey[key]
		return v, ok
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok = s.byKey[key]
	return v, ok
}

func (s *textStore) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("loc: status %d", res.StatusCode)
	}

	// 言語ごとに分かれた map なので、必要な言語だけ取り出す。全部保持すると
	// 使わない 16 言語分がメモリに残る。
	var parsed map[string]map[string]string
	if err := json.NewDecoder(io.LimitReader(res.Body, maxLocBytes)).Decode(&parsed); err != nil {
		return err
	}
	picked, ok := parsed[s.lang]
	if !ok {
		// 設定された言語が無ければ英語に落とす (取得元は必ず en を持つ)。
		picked = parsed["en"]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey = picked
	s.fetched = time.Now()
	return nil
}

// relicSetStore maps an artifact set id to the text key of its name.
//
// **ハッシュの体系が 2 つある。** Enka の応答が持つ `setNameTextMapHash` は
// loc.json 側のキーで、古いセットしか載っていない (実測でユーザーの装備の
// 6 割が引けなかった)。一方 gi/relics.json の `Sets[].Name` は gi/locs.json
// 側のキーで、こちらは全 65 セットが揃っている。応答のハッシュで引けなかった
// ときに、この表を通して引き直す。
//
// 保持するのは Sets だけ (65 件)。Items は数千件あるが、set id は
// アイコン名 `UI_RelicIcon_<setId>_<部位>` から取れるので要らない。
type relicSetStore struct {
	mu      sync.RWMutex
	byID    map[string]string
	fetched time.Time
	client  *http.Client
}

func newRelicSetStore(client *http.Client) *relicSetStore {
	return &relicSetStore{client: client}
}

// relicSetIDPattern extracts the set id from an artifact icon name.
var relicSetIDPattern = regexp.MustCompile(`^UI_RelicIcon_(\d+)_\d+$`)

// SetKeyFromIcon returns the localisation key for the set an icon belongs to.
func (s *relicSetStore) SetKeyFromIcon(ctx context.Context, icon string) (string, bool) {
	if s == nil {
		return "", false
	}
	m := relicSetIDPattern.FindStringSubmatch(icon)
	if m == nil {
		return "", false
	}
	id := m[1]

	s.mu.RLock()
	fresh := time.Since(s.fetched) < 24*time.Hour && s.byID != nil
	key, ok := s.byID[id]
	s.mu.RUnlock()
	if fresh {
		return key, ok
	}
	if err := s.refresh(ctx); err != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		key, ok := s.byID[id]
		return key, ok
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok = s.byID[id]
	return key, ok
}

func (s *relicSetStore) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relicsURL, nil)
	if err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("relics.json: status %d", res.StatusCode)
	}

	// Items は使わないが、同じファイルに入っているので読み飛ばす形で受ける。
	var parsed struct {
		Sets map[string]struct {
			Name json.RawMessage `json:"Name"`
		} `json:"Sets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxLocBytes)).Decode(&parsed); err != nil {
		return err
	}

	byID := make(map[string]string, len(parsed.Sets))
	for id, v := range parsed.Sets {
		// Name は文字列で入っていることも数値のこともあるので、素の JSON から
		// 引用符だけ外す。
		byID[id] = strings.Trim(string(v.Name), `"`)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID = byID
	s.fetched = time.Now()
	return nil
}

// fetchAsset downloads one UI image from Enka.
func fetchAsset(ctx context.Context, client *http.Client, userAgent, name string) ([]byte, string, error) {
	if !assetNamePattern(name) {
		return nil, "", &upstreamError{status: http.StatusBadRequest, msg: "asset 名が不正です"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetBase+name+".png", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)

	res, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return nil, "", &upstreamError{status: res.StatusCode, msg: fmt.Sprintf("asset: status %d", res.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxAssetBytes))
	if err != nil {
		return nil, "", err
	}

	// **取得元の Content-Type をそのまま流さない。** 扱うのは PNG だけと
	// 決めているので、こちらで固定する。取得元が別の型を名乗ってもブラウザに
	// 渡らない。
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		return nil, "", &upstreamError{status: http.StatusBadGateway, msg: "画像ではない応答が返りました"}
	}
	return body, "image/png", nil
}

// assetURL builds the same-origin proxy URL for a UI image.
func assetURL(name string) string {
	if name == "" {
		return ""
	}
	return "/api/plugin/genshin/asset/" + name
}

// nameCardURL maps a namecard id to its proxied picture.
//
// 未知の id では空を返す。名刺が引けなくてもカードは無地の背景で成立させる
// (画像が出ないだけで、表示そのものは壊さない)。
func nameCardURL(ctx context.Context, store *characterStore, id int) string {
	if id == 0 {
		return ""
	}
	info, ok := store.Lookup(ctx, id)
	if !ok {
		return ""
	}
	return assetURL(info.IconName())
}

// spiralLabel formats the Spiral Abyss progress as "8-3".
//
// 未到達 (0) のときは空にする。「0-0」と出すと到達したように見える。
func spiralLabel(floor, level int) string {
	if floor <= 0 {
		return ""
	}
	return strconv.Itoa(floor) + "-" + strconv.Itoa(level)
}

// theaterLabel formats 幻想シアター progress as "第10幕 (難易度3)".
//
// 未挑戦 (0) では空にする。螺旋と同じ理由で「0」と出すと挑んだように見える。
func theaterLabel(act, mode int) string {
	if act <= 0 {
		return ""
	}
	s := "第" + strconv.Itoa(act) + "幕"
	if mode > 0 {
		s += " (難易度" + strconv.Itoa(mode) + ")"
	}
	return s
}
