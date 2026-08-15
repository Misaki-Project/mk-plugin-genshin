package genshin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
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

/*
 * Enka が公開しているマスターデータ。
 *
 * # store/ 直下 (旧形式) を使わないこと
 *
 * 同じ内容が `store/characters.json` と `store/gi/avatars.json` の 2 系統で
 * 置かれているが、**旧形式は更新が止まっている**。
 *
 *	store/characters.json   2025-12-07 (6.2) で停止
 *	store/loc.json          2026-01-01 で停止
 *	store/namecards.json    2025-12-07 で停止
 *	store/gi/*.json         更新が続いている
 *
 * 旧形式のままだと 6.2 以降に追加されたキャラ (コロンビーナ、サンドローネ、
 * アリョーシャなど 11 人) が丸ごと欠け、名前もアイコンも出せない。ドールに
 * 至ってはエントリが空オブジェクトになっている (mk-plugin-genshin #1)。
 *
 * 新形式は**アイコンをパスで持つ** (`/ui/UI_AvatarIcon_Side_Ambor.png`)。
 * 拡張子まで含まれるので、名刺の `.jpg` も正しく扱える。
 */
const (
	avatarsURL   = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/gi/avatars.json"
	namecardsURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/gi/namecards.json"
	// locsURL maps text hashes to display names, and UI keys like
	// `FIGHT_PROP_CRITICAL` to labels.
	//
	// **旧形式では取得元が 3 つに割れていた** (キャラ名は loc.json、武器名と
	// 一部のセット名は gi/locs.json、新しい聖遺物セットは gi/relics.json 経由)。
	// 新形式はこれ 1 つで全部引けるので、その分岐は要らなくなった。
	locsURL = "https://raw.githubusercontent.com/EnkaNetwork/API-docs/master/store/gi/locs.json"
	// relicsURL carries the artifact set masters. 応答の setNameTextMapHash で
	// 引けないセットを、アイコン名から set id を取って補うのに使う。
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
//
// 新形式は拡張子まで持つ (`UI_NameCardPic_0_P.jpg`) のでドットを許すが、
// **`..` と先頭のドットは弾く** — そこを通すと親ディレクトリへ辿れる。
var assetNamePattern = func() func(string) bool {
	return func(s string) bool {
		if s == "" || len(s) > 128 {
			return false
		}
		if strings.HasPrefix(s, ".") || strings.Contains(s, "..") || strings.Contains(s, "/") {
			return false
		}
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '_' || r == '-' || r == '.':
			default:
				return false
			}
		}
		return true
	}
}()

// assetFileName appends the default extension when the name has none.
//
// **移行前に保存されたスナップショットは拡張子を持たない** (`UI_AvatarIcon_Venti`)。
// 新形式のマスターは拡張子込みのパスを持つが、DB に残っている古いデータは
// そうではないので、補わないと ttl が切れて取り直すまでの間そこだけ画像が
// 出なくなる (実際に踏んだ)。
func assetFileName(name string) string {
	if path.Ext(name) == "" {
		return name + ".png"
	}
	return name
}

// assetPathPrefix is where Enka's master data points at UI images.
//
// 新形式は `/ui/UI_AvatarIcon_Side_Ambor.png` のようなパスを持つ。中継の鍵に
// するのはこの前置きを剥がした部分。
const assetPathPrefix = "/ui/"

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
//
// **新形式はパスで来る** (`/ui/UI_AvatarIcon_Side_Ambor.png`) ので前置きを
// 剥がす。拡張子はそのまま残す — 名刺は `.jpg` で、`.png` 決め打ちにすると
// 取り違える。
func (c characterInfo) IconName() string {
	name := c.SideIconName
	if name == "" {
		name = c.Icon
	}
	name = strings.TrimPrefix(name, assetPathPrefix)
	if name == "" {
		return ""
	}
	return strings.Replace(name, "_Side_", "_", 1)
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
	return &characterStore{client: client, endpoint: avatarsURL}
}

// newNamecardStore reads the namecard id -> picture mapping.
func newNamecardStore(client *http.Client) *characterStore {
	return &characterStore{client: client, endpoint: namecardsURL}
}

// Lookup returns the character info for an avatarId.
//
// skillDepotID が 0 でなければ `<avatarId>-<skillDepotId>` を先に試す。
// **旅人とドールは元素ごとに別のエントリを持つ** ので、これを見ないと元素も
// 天賦も引けない (旧形式では `10000117` が空オブジェクトだった)。
//
// **nil レシーバでも落とさない。** master を配線し忘れても、名前やアイコンが
// 出ないだけでカードは成立させる。
func (s *characterStore) Lookup(ctx context.Context, avatarID, skillDepotID int) (characterInfo, bool) {
	if s == nil {
		return characterInfo{}, false
	}
	keys := lookupKeys(avatarID, skillDepotID)

	s.mu.RLock()
	fresh := time.Since(s.fetched) < 24*time.Hour && s.byID != nil
	info, ok := pick(s.byID, keys)
	s.mu.RUnlock()

	if fresh {
		return info, ok
	}
	if err := s.refresh(ctx); err != nil {
		// 取り直せなくても、古いものが手元にあればそれを使う。キャラ名が
		// 出ないだけでカード自体は表示できる方がよい。
		s.mu.RLock()
		defer s.mu.RUnlock()
		return pick(s.byID, keys)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return pick(s.byID, keys)
}

// lookupKeys lists the master keys to try, most specific first.
func lookupKeys(avatarID, skillDepotID int) []string {
	id := strconv.Itoa(avatarID)
	if skillDepotID == 0 {
		return []string{id}
	}
	return []string{id + "-" + strconv.Itoa(skillDepotID), id}
}

// pick returns the first key that resolves.
//
// **元素なしのエントリにも落ちる。** 旅人の `10000005` は新形式では
// Element が "None" になるが、名前とアイコンは引けるので出せるものは出す。
func pick(byID map[string]characterInfo, keys []string) (characterInfo, bool) {
	for _, k := range keys {
		if v, ok := byID[k]; ok {
			return v, true
		}
	}
	return characterInfo{}, false
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

	// **拡張子は name に含まれている。** 決め打ちで足すと、名刺 (.jpg) を
	// .png として要求することになる。
	//
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetBase+assetFileName(name), nil)
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

	// **取得元の Content-Type をそのまま流さない。** 画像であることだけ確かめ、
	// 返す型はこちらで決める。取得元が別の型を名乗ってもブラウザには渡らない。
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", &upstreamError{status: http.StatusBadGateway, msg: "画像ではない応答が返りました"}
	}
	return body, normalizeImageType(ct), nil
}

// normalizeImageType maps an upstream Content-Type onto one we serve.
//
// 名刺は JPEG、アイコンは PNG。**PNG 決め打ちで返すと名刺が化ける**ので、
// 扱うと決めた型の中から選ぶ。
func normalizeImageType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "image/jpeg"), strings.HasPrefix(ct, "image/jpg"):
		return "image/jpeg"
	case strings.HasPrefix(ct, "image/webp"):
		return "image/webp"
	case strings.HasPrefix(ct, "image/gif"):
		return "image/gif"
	default:
		return "image/png"
	}
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
	info, ok := store.Lookup(ctx, id, 0)
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
