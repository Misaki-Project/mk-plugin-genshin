package genshin

import (
	"context"
	"strconv"
	"strings"
)

/*
 * ショーケースのキャラクター 1 体分を、表示できる形に組み立てる。
 *
 * Enka の avatarInfoList はゲーム内部の表現そのままで、名前もステータス名も
 * ID / ハッシュでしか入っていない。ここで master data (characters.json /
 * loc.json) を引いて、frontend がそのまま描ける形に均す。
 *
 * **値の単位に 2 系統ある。**
 *   - fightPropMap (キャラの実数ステータス) の割合系は 0-1 の小数
 *     (会心率 0.5194 = 51.9%)
 *   - flat の statValue (武器・聖遺物) は表示用の値がそのまま
 *     (FIGHT_PROP_ATTACK_PERCENT: 11.1 = 11.1%)
 * 混ぜると会心率が 5194% になったりするので、変換は前者だけに掛ける。
 */

// character is one showcased character with its full build.
type character struct {
	AvatarID      int        `json:"avatarId"`
	Name          string     `json:"name"`
	Icon          string     `json:"icon"`
	Element       string     `json:"element"`
	Level         int        `json:"level"`
	Ascension     int        `json:"ascension"`
	Constellation int        `json:"constellation"`
	ConstIcons    []string   `json:"constIcons"`
	Friendship    int        `json:"friendship"`
	Talents       []talent   `json:"talents"`
	Weapon        *weapon    `json:"weapon,omitempty"`
	Artifacts     []artifact `json:"artifacts"`
	Stats         []stat     `json:"stats"`
}

// talent is one of the three talents, in the order the game shows them.
type talent struct {
	Icon  string `json:"icon"`
	Level int    `json:"level"`
	// Extra is the bonus from constellations (通常は 0 か 3)。
	Extra int `json:"extra"`
}

type weapon struct {
	Name   string `json:"name"`
	Icon   string `json:"icon"`
	Rarity int    `json:"rarity"`
	Level  int    `json:"level"`
	// Refine is the 精錬ランク (1-5)。affixMap は 0 始まりなので +1 する。
	Refine int    `json:"refine"`
	Stats  []stat `json:"stats"`
}

type artifact struct {
	Slot    string `json:"slot"`
	Name    string `json:"name"`
	SetName string `json:"setName"`
	Icon    string `json:"icon"`
	Rarity  int    `json:"rarity"`
	Level   int    `json:"level"`
	Main    stat   `json:"main"`
	Subs    []stat `json:"subs"`
}

// stat is one labelled number. Percent tells the frontend to append "%".
type stat struct {
	Label   string  `json:"label"`
	Value   float64 `json:"value"`
	Percent bool    `json:"percent"`
}

// --- Enka の生の形 ---

type rawAvatar struct {
	AvatarID int `json:"avatarId"`
	PropMap  map[string]struct {
		Val string `json:"val"`
	} `json:"propMap"`
	FightPropMap            map[string]float64 `json:"fightPropMap"`
	TalentIDList            []int              `json:"talentIdList"`
	SkillLevelMap           map[string]int     `json:"skillLevelMap"`
	ProudSkillExtraLevelMap map[string]int     `json:"proudSkillExtraLevelMap"`
	FetterInfo              struct {
		ExpLevel int `json:"expLevel"`
	} `json:"fetterInfo"`
	EquipList []rawEquip `json:"equipList"`
}

type rawEquip struct {
	Weapon *struct {
		Level    int            `json:"level"`
		AffixMap map[string]int `json:"affixMap"`
	} `json:"weapon"`
	Reliquary *struct {
		Level int `json:"level"`
	} `json:"reliquary"`
	Flat struct {
		// **ドキュメントは nameTextHashMap と書いているが実際は
		// nameTextMapHash。** 実レスポンスに合わせる。
		NameTextMapHash    string `json:"nameTextMapHash"`
		SetNameTextMapHash string `json:"setNameTextMapHash"`
		RankLevel          int    `json:"rankLevel"`
		ItemType           string `json:"itemType"`
		Icon               string `json:"icon"`
		EquipType          string `json:"equipType"`
		ReliquaryMainstat  *struct {
			MainPropID string  `json:"mainPropId"`
			StatValue  float64 `json:"statValue"`
		} `json:"reliquaryMainstat"`
		ReliquarySubstats []rawStat `json:"reliquarySubstats"`
		WeaponStats       []rawStat `json:"weaponStats"`
	} `json:"flat"`
}

type rawStat struct {
	AppendPropID string  `json:"appendPropId"`
	StatValue    float64 `json:"statValue"`
}

// --- 表示定義 ---

// shownFightProps lists the character stats we surface, in display order.
//
// fightPropMap のキーは数値 ID なので、ラベルを引くための FIGHT_PROP 名を
// 対にしておく (ラベル自体は loc から取るので、ここに日本語は持たない)。
var shownFightProps = []struct {
	id  string
	key string
	// zeroOK keeps the row even when the value is 0. 基礎ステータスは 0 でも
	// 出すが、元素ダメージバフは付いていなければ行ごと省く。
	zeroOK bool
}{
	{"2000", "FIGHT_PROP_HP", true},
	{"2001", "FIGHT_PROP_ATTACK", true},
	{"2002", "FIGHT_PROP_DEFENSE", true},
	{"28", "FIGHT_PROP_ELEMENT_MASTERY", true},
	{"20", "FIGHT_PROP_CRITICAL", true},
	{"22", "FIGHT_PROP_CRITICAL_HURT", true},
	{"23", "FIGHT_PROP_CHARGE_EFFICIENCY", true},
	{"26", "FIGHT_PROP_HEAL_ADD", false},
	{"30", "FIGHT_PROP_PHYSICAL_ADD_HURT", false},
	{"40", "FIGHT_PROP_FIRE_ADD_HURT", false},
	{"41", "FIGHT_PROP_ELEC_ADD_HURT", false},
	{"42", "FIGHT_PROP_WATER_ADD_HURT", false},
	{"43", "FIGHT_PROP_GRASS_ADD_HURT", false},
	{"44", "FIGHT_PROP_WIND_ADD_HURT", false},
	{"45", "FIGHT_PROP_ROCK_ADD_HURT", false},
	{"46", "FIGHT_PROP_ICE_ADD_HURT", false},
}

// equipSlots maps Enka's equip type to the short slot name.
// loc には部位名が無いので、ここだけ自前で持つ (5 種類で固定)。
var equipSlots = map[string]string{
	"EQUIP_BRACER":   "花",
	"EQUIP_NECKLACE": "羽",
	"EQUIP_SHOES":    "砂",
	"EQUIP_RING":     "杯",
	"EQUIP_DRESS":    "冠",
}

// isPercentProp reports whether a FIGHT_PROP is shown as a percentage.
func isPercentProp(key string) bool {
	switch key {
	case "FIGHT_PROP_CRITICAL", "FIGHT_PROP_CRITICAL_HURT",
		"FIGHT_PROP_CHARGE_EFFICIENCY", "FIGHT_PROP_HEAL_ADD":
		return true
	}
	return strings.HasSuffix(key, "_PERCENT") || strings.HasSuffix(key, "_ADD_HURT")
}

// --- 組み立て ---

// buildCharacter turns one raw avatar into its display form.
//
// master data が引けない項目は空のまま返す。名前が出ないだけで、レベルや
// ステータスは表示できる方がよい (取得元の master が一時的に落ちても
// カードが空にならない)。
func (c *enkaClient) buildCharacter(ctx context.Context, raw rawAvatar) character {
	out := character{
		AvatarID:   raw.AvatarID,
		Level:      propInt(raw.PropMap, "4001"),
		Ascension:  propInt(raw.PropMap, "1002"),
		Friendship: raw.FetterInfo.ExpLevel,
		// talentIdList は C0 だと存在しない (undefined)。
		Constellation: len(raw.TalentIDList),
		Artifacts:     []artifact{},
		Talents:       []talent{},
		Stats:         []stat{},
		ConstIcons:    []string{},
	}

	info, hasInfo := c.chars.Lookup(ctx, raw.AvatarID)
	if hasInfo {
		out.Icon = assetURL(info.IconName())
		out.Element = info.Element
		if name, ok := c.text(ctx, strconv.FormatInt(info.NameTextMapHash, 10)); ok {
			out.Name = name
		}
		// 解放済みの星座アイコンだけを、解放順に並べる。
		for i, icon := range info.Consts {
			if i >= out.Constellation {
				break
			}
			out.ConstIcons = append(out.ConstIcons, assetURL(icon))
		}
		// SkillOrder はゲームの表示順 (通常 / スキル / 爆発)。map の反復順は
		// 不定なので、必ずこの順で並べる。
		for _, sid := range info.SkillOrder {
			key := strconv.Itoa(sid)
			lv, ok := raw.SkillLevelMap[key]
			if !ok {
				continue
			}
			t := talent{Level: lv, Icon: assetURL(info.Skills[key])}
			// 命ノ星座による強化は proudSkillGroupId 側に入るので、
			// skill id から引き直す。
			if pid, ok := info.ProudMap[key]; ok {
				t.Extra = raw.ProudSkillExtraLevelMap[strconv.Itoa(pid)]
			}
			out.Talents = append(out.Talents, t)
		}
	}

	for _, p := range shownFightProps {
		v := raw.FightPropMap[p.id]
		if v == 0 && !p.zeroOK {
			continue
		}
		pct := isPercentProp(p.key)
		if pct {
			// fightPropMap の割合系だけ 0-1 で来る。
			v *= 100
		}
		out.Stats = append(out.Stats, stat{Label: c.label(ctx, p.key), Value: round1(v), Percent: pct})
	}

	for _, e := range raw.EquipList {
		switch {
		case e.Weapon != nil:
			w := &weapon{
				Icon:   assetURL(e.Flat.Icon),
				Rarity: e.Flat.RankLevel,
				Level:  e.Weapon.Level,
				Refine: 1,
				Stats:  []stat{},
			}
			if name, ok := c.text(ctx, e.Flat.NameTextMapHash); ok {
				w.Name = name
			}
			// affixMap は 1 件だけ入っていて、値が 0-4 (= 精錬 1-5)。
			for _, v := range e.Weapon.AffixMap {
				w.Refine = v + 1
			}
			for _, s := range e.Flat.WeaponStats {
				w.Stats = append(w.Stats, c.flatStat(ctx, s))
			}
			out.Weapon = w
		case e.Reliquary != nil:
			a := artifact{
				Slot:   equipSlots[e.Flat.EquipType],
				Icon:   assetURL(e.Flat.Icon),
				Rarity: e.Flat.RankLevel,
				Level:  e.Reliquary.Level,
				Subs:   []stat{},
			}
			if name, ok := c.text(ctx, e.Flat.NameTextMapHash); ok {
				a.Name = name
			}
			if set, ok := c.setName(ctx, e.Flat.SetNameTextMapHash, e.Flat.Icon); ok {
				a.SetName = set
			}
			if m := e.Flat.ReliquaryMainstat; m != nil {
				a.Main = c.flatStat(ctx, rawStat{AppendPropID: m.MainPropID, StatValue: m.StatValue})
			}
			for _, s := range e.Flat.ReliquarySubstats {
				a.Subs = append(a.Subs, c.flatStat(ctx, s))
			}
			out.Artifacts = append(out.Artifacts, a)
		}
	}

	return out
}

// flatStat converts a weapon / artifact stat. **こちらは変換しない** —
// statValue が既に表示用の値。
func (c *enkaClient) flatStat(ctx context.Context, s rawStat) stat {
	return stat{
		Label:   c.label(ctx, s.AppendPropID),
		Value:   round1(s.StatValue),
		Percent: isPercentProp(s.AppendPropID),
	}
}

// setName resolves an artifact's set name.
//
// 応答の setNameTextMapHash は loc.json 側のキーだが、**新しいセットは
// そこに載っていない** (実測で 4 割しか引けなかった)。引けなければアイコン名
// から set id を取り、gi/relics.json → gi/locs.json の経路で引き直す。
func (c *enkaClient) setName(ctx context.Context, hash, icon string) (string, bool) {
	if v, ok := c.text(ctx, hash); ok {
		return v, true
	}
	key, ok := c.relicSets.SetKeyFromIcon(ctx, icon)
	if !ok {
		return "", false
	}
	return c.text(ctx, key)
}

// text resolves a hash or UI key against both localisation files.
//
// **2 つは収録範囲が違う。** 実測 (2026-08) では、聖遺物セット名は
// loc.json に 1/65 しか無く gi/locs.json に 65/65、武器名は 131/249 と
// 249/249、逆にキャラ名は 128/134 と 60/134。どちらか片方だけを見ると
// 名前が半分出ない。共通キー 236 件は値が完全に一致しているので、順に
// 引いて先に見つかった方を使ってよい。
func (c *enkaClient) text(ctx context.Context, key string) (string, bool) {
	if v, ok := c.texts.Lookup(ctx, key); ok && v != "" {
		return v, true
	}
	if v, ok := c.uiTexts.Lookup(ctx, key); ok && v != "" {
		return v, true
	}
	return "", false
}

// label resolves a FIGHT_PROP key to its display name, falling back to the
// key itself so「何のステータスか分からない行」にはならない。
func (c *enkaClient) label(ctx context.Context, key string) string {
	if v, ok := c.text(ctx, key); ok {
		return v
	}
	return key
}

// propInt reads propMap's string-typed value.
func propInt(m map[string]struct {
	Val string `json:"val"`
}, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v.Val)
	if err != nil {
		return 0
	}
	return n
}

// round1 keeps one decimal place. 表示に使うだけなので、ここで丸めて
// 保存量と描画のばらつきを抑える。
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
