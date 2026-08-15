package genshin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// masterServer serves canned copies of Enka's static master data.
//
// **新形式 (store/gi/*.json) の形をなぞる。** アイコンはパスで来て拡張子まで
// 含み、テキストは 1 ファイルに統合されている。旧形式の形でテストを書くと、
// 移行できているかを検証できない (mk-plugin-genshin #1)。
func masterServer(t *testing.T) *httptest.Server {
	t.Helper()
	avatars := `{
		"10000002":{"Element":"Ice","Consts":["/ui/C1.png","/ui/C2.png","/ui/C3.png","/ui/C4.png","/ui/C5.png","/ui/C6.png"],
			"SkillOrder":[10024,10018,10019],
			"Skills":{"10018":"/ui/Skill_S.png","10019":"/ui/Skill_E.png","10024":"/ui/Skill_A.png"},
			"ProudMap":{"10018":232,"10019":239,"10024":231},
			"NameTextMapHash":1006042610,"SideIconName":"/ui/UI_AvatarIcon_Side_Ayaka.png"},
		"10000117":{},
		"10000117-11701":{"Element":"Fire","Consts":[],"SkillOrder":[],"Skills":{},"ProudMap":{},
			"NameTextMapHash":1496871274,"SideIconName":"/ui/UI_AvatarIcon_Side_MannequinBoy.png"}
	}`
	// 新形式は 1 ファイルにキャラ名・武器名・セット名・ステータス名が揃う。
	locs := `{"ja":{
		"1006042610":"神里綾華","1496871274":"ドール（男）",
		"111":"天空の翼","222":"旧貴族のしつけ","333":"血染めの騎士道","444":"諧律奇想の断章",
		"FIGHT_PROP_HP":"HP","FIGHT_PROP_CRITICAL":"会心率",
		"FIGHT_PROP_ATTACK":"攻撃力","FIGHT_PROP_DEFENSE":"防御力",
		"FIGHT_PROP_ELEMENT_MASTERY":"元素熟知","FIGHT_PROP_CRITICAL_HURT":"会心ダメージ",
		"FIGHT_PROP_CHARGE_EFFICIENCY":"元素チャージ効率","FIGHT_PROP_ICE_ADD_HURT":"氷元素ダメージ",
		"FIGHT_PROP_BASE_ATTACK":"基礎攻撃力","FIGHT_PROP_ATTACK_PERCENT":"攻撃力"},"en":{}}`

	mux := http.NewServeMux()
	mux.HandleFunc("/avatars.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(avatars)) })
	mux.HandleFunc("/locs.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(locs)) })
	// 名刺は .jpg。**PNG 決め打ちにすると化ける。**
	mux.HandleFunc("/namecards.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"210042":{"Icon":"/ui/UI_NameCardPic_Ayaka_P.jpg"}}`))
	})
	mux.HandleFunc("/relics.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Items":{},"Sets":{"15008":{"Name":"333"},"15035":{"Name":"444"}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clientWithMasters(t *testing.T, enkaURL string) *enkaClient {
	t.Helper()
	m := masterServer(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	return &enkaClient{
		set:       settings{Endpoint: enkaURL, UserAgent: "test/1.0", TimeoutSeconds: 5, Language: "ja"},
		http:      hc,
		chars:     &characterStore{client: hc, endpoint: m.URL + "/avatars.json"},
		namecards: &characterStore{client: hc, endpoint: m.URL + "/namecards.json"},
		// 新形式は 1 ファイルなので、どちらも同じ取得元を指す。
		texts:     &textStore{client: hc, endpoint: m.URL + "/locs.json", lang: "ja"},
		uiTexts:   &textStore{client: hc, endpoint: m.URL + "/locs.json", lang: "ja"},
		relicSets: &relicSetStore{client: hc, byID: map[string]string{"15008": "333", "15035": "444"}, fetched: time.Now()},
	}
}

// 実レスポンスに合わせた 1 体分。値は Enka の実データの形をなぞっている。
const sampleAvatar = `{
	"avatarId": 10000002,
	"propMap": {"1002": {"type":1002,"val":"6"}, "4001": {"type":4001,"val":"90"}},
	"talentIdList": [211,212,213],
	"fightPropMap": {"2000":14241.17,"2001":2367.04,"2002":951.73,"20":0.5194,"22":1.9391,"23":1,"28":39.62,"46":0.616,"40":0},
	"skillLevelMap": {"10017":1,"10018":10,"10019":9,"10024":8},
	"proudSkillExtraLevelMap": {"239":3},
	"fetterInfo": {"expLevel": 10},
	"equipList": [
		{"itemId":1,"reliquary":{"level":21},"flat":{"nameTextMapHash":"0","setNameTextMapHash":"222","rankLevel":5,
			"itemType":"ITEM_RELIQUARY","icon":"UI_RelicIcon_15008_4","equipType":"EQUIP_BRACER",
			"reliquaryMainstat":{"mainPropId":"FIGHT_PROP_HP","statValue":4780},
			"reliquarySubstats":[{"appendPropId":"FIGHT_PROP_ATTACK_PERCENT","statValue":11.1},
				{"appendPropId":"FIGHT_PROP_CRITICAL","statValue":3.9}]}},
		{"itemId":3,"reliquary":{"level":20},"flat":{"nameTextMapHash":"0","setNameTextMapHash":"999","rankLevel":5,
			"itemType":"ITEM_RELIQUARY","icon":"UI_RelicIcon_15035_5","equipType":"EQUIP_SHOES",
			"reliquaryMainstat":{"mainPropId":"FIGHT_PROP_ATTACK_PERCENT","statValue":46.6},
			"reliquarySubstats":[]}},
		{"itemId":2,"weapon":{"level":90,"affixMap":{"115501":4}},"flat":{"nameTextMapHash":"111","rankLevel":5,
			"itemType":"ITEM_WEAPON","icon":"UI_EquipIcon_Bow_Dvalin",
			"weaponStats":[{"appendPropId":"FIGHT_PROP_BASE_ATTACK","statValue":674},
				{"appendPropId":"FIGHT_PROP_CRITICAL","statValue":22.1}]}}
	]
}`

func buildSample(t *testing.T) character {
	t.Helper()
	var raw rawAvatar
	if err := json.Unmarshal([]byte(sampleAvatar), &raw); err != nil {
		t.Fatal(err)
	}
	return clientWithMasters(t, "").buildCharacter(context.Background(), raw)
}

func TestBuildCharacter_Basics(t *testing.T) {
	c := buildSample(t)

	if c.Name != "神里綾華" {
		t.Errorf("キャラ名が解決できていない: %q", c.Name)
	}
	if c.Element != "Ice" {
		t.Errorf("元素: %q", c.Element)
	}
	if c.Level != 90 || c.Ascension != 6 {
		t.Errorf("レベル/突破: %d / %d", c.Level, c.Ascension)
	}
	// talentIdList の件数がそのまま命ノ星座の数。
	if c.Constellation != 3 || len(c.ConstIcons) != 3 {
		t.Errorf("命ノ星座: %d / アイコン %d 件", c.Constellation, len(c.ConstIcons))
	}
	if c.Friendship != 10 {
		t.Errorf("好感度: %d", c.Friendship)
	}
	// **新形式は拡張子まで含む。** 旧形式は名前だけだった。
	if c.Icon != "/api/plugin/genshin/asset/UI_AvatarIcon_Ayaka.png" {
		t.Errorf("アイコンが proxy 経由でない: %q", c.Icon)
	}
}

// 天賦は SkillOrder の順 (通常 → スキル → 爆発)。map の反復順に依存しては
// いけないので、並びを固定する。
func TestBuildCharacter_TalentsFollowSkillOrder(t *testing.T) {
	c := buildSample(t)

	if len(c.Talents) != 3 {
		t.Fatalf("天賦が 3 つでない: %d", len(c.Talents))
	}
	want := []int{8, 10, 9} // 10024, 10018, 10019 の順
	for i, w := range want {
		if c.Talents[i].Level != w {
			t.Errorf("%d 番目の天賦レベル: got %d want %d", i, c.Talents[i].Level, w)
		}
	}
	// 命ノ星座による +3 は proudSkillGroupId 側に入る (10019 -> 239)。
	if c.Talents[2].Extra != 3 {
		t.Errorf("星座による加算が反映されていない: %d", c.Talents[2].Extra)
	}
	if c.Talents[0].Extra != 0 {
		t.Errorf("加算の無い天賦に値が付いている: %d", c.Talents[0].Extra)
	}
}

// **単位の取り違えを防ぐ。** fightPropMap の割合は 0-1、flat の statValue は
// 表示用の値。混ぜると会心率が 5194% になる。
func TestBuildCharacter_PercentConversion(t *testing.T) {
	c := buildSample(t)

	byLabel := map[string]stat{}
	for _, s := range c.Stats {
		byLabel[s.Label] = s
	}

	if got := byLabel["会心率"]; got.Value != 51.9 || !got.Percent {
		t.Errorf("会心率 (fightPropMap は 0-1): %+v", got)
	}
	if got := byLabel["HP"]; got.Value != 14241.2 || got.Percent {
		t.Errorf("HP は割合ではない: %+v", got)
	}
	if got := byLabel["元素熟知"]; got.Value != 39.6 || got.Percent {
		t.Errorf("元素熟知: %+v", got)
	}
	// 武器・聖遺物側は変換しない。
	if c.Weapon == nil {
		t.Fatal("武器が取れていない")
	}
	var crit stat
	for _, s := range c.Weapon.Stats {
		if s.Label == "会心率" {
			crit = s
		}
	}
	if crit.Value != 22.1 || !crit.Percent {
		t.Errorf("武器のサブステは変換しない: %+v", crit)
	}
}

// 0 のダメージバフは行ごと省き、付いているものだけ出す。
func TestBuildCharacter_SkipsZeroBonuses(t *testing.T) {
	c := buildSample(t)

	labels := map[string]bool{}
	for _, s := range c.Stats {
		labels[s.Label] = true
	}
	if !labels["氷元素ダメージ"] {
		t.Error("付いている氷元素ダメージが出ていない")
	}
	if labels["炎元素ダメージ"] {
		t.Error("0 の炎元素ダメージを出している")
	}
}

func TestBuildCharacter_Weapon(t *testing.T) {
	c := buildSample(t)

	if c.Weapon == nil {
		t.Fatal("武器が取れていない")
	}
	if c.Weapon.Name != "天空の翼" {
		t.Errorf("武器名: %q", c.Weapon.Name)
	}
	// affixMap は 0 始まり。R5 は 4 で入っている。
	if c.Weapon.Refine != 5 {
		t.Errorf("精錬ランク: %d (affixMap の 4 は R5)", c.Weapon.Refine)
	}
	if c.Weapon.Level != 90 || c.Weapon.Rarity != 5 {
		t.Errorf("レベル/レア度: %d / %d", c.Weapon.Level, c.Weapon.Rarity)
	}
}

func TestBuildCharacter_Artifact(t *testing.T) {
	c := buildSample(t)

	if len(c.Artifacts) != 2 {
		t.Fatalf("聖遺物の件数: %d", len(c.Artifacts))
	}
	a := c.Artifacts[0]
	if a.Slot != "花" {
		t.Errorf("部位: %q", a.Slot)
	}
	if a.SetName != "旧貴族のしつけ" {
		t.Errorf("セット名: %q", a.SetName)
	}
	if a.Level != 21 || a.Rarity != 5 {
		t.Errorf("レベル/レア度: %d / %d", a.Level, a.Rarity)
	}
	if a.Main.Label != "HP" || a.Main.Value != 4780 || a.Main.Percent {
		t.Errorf("メインステータス: %+v", a.Main)
	}
	if len(a.Subs) != 2 {
		t.Fatalf("サブステの件数: %d", len(a.Subs))
	}
	if !a.Subs[0].Percent || a.Subs[0].Value != 11.1 {
		t.Errorf("攻撃力%% のサブステ: %+v", a.Subs[0])
	}
}

// master が引けなくても、レベルやステータスは出す。
func TestBuildCharacter_WithoutMasters(t *testing.T) {
	var raw rawAvatar
	if err := json.Unmarshal([]byte(sampleAvatar), &raw); err != nil {
		t.Fatal(err)
	}
	// store を配線していない client (nil レシーバ) でも落ちないこと。
	c := (&enkaClient{}).buildCharacter(context.Background(), raw)

	if c.Level != 90 {
		t.Errorf("レベルが取れない: %d", c.Level)
	}
	if c.Name != "" || c.Icon != "" {
		t.Errorf("master 無しで名前/アイコンが埋まっている: %q / %q", c.Name, c.Icon)
	}
	if len(c.Stats) == 0 {
		t.Error("ステータスが出ていない")
	}
	// ラベルが引けないときは key をそのまま出す (何の行か分かるように)。
	if c.Stats[0].Label != "FIGHT_PROP_HP" {
		t.Errorf("ラベルの fallback: %q", c.Stats[0].Label)
	}
}

// 名前の取得元は 2 ファイルに分かれている。片方にしか無いものも引けること。
func TestBuildCharacter_ResolvesNamesFromBothLocFiles(t *testing.T) {
	c := buildSample(t)

	// loc.json 側にしか無い
	if c.Name != "神里綾華" {
		t.Errorf("キャラ名 (loc.json 側): %q", c.Name)
	}
	// gi/locs.json 側にしか無い
	if c.Weapon == nil || c.Weapon.Name != "天空の翼" {
		t.Errorf("武器名 (gi/locs.json 側): %+v", c.Weapon)
	}
	if len(c.Artifacts) == 0 || c.Artifacts[0].SetName != "旧貴族のしつけ" {
		t.Errorf("聖遺物セット名 (loc.json 側): %+v", c.Artifacts)
	}
}

// 新しいセットは loc.json に setNameTextMapHash が無い。アイコン名から
// set id を取って gi/relics.json → gi/locs.json で引き直すこと。
func TestBuildCharacter_ResolvesNewSetViaRelicMaster(t *testing.T) {
	c := buildSample(t)

	var shoes artifact
	for _, a := range c.Artifacts {
		if a.Slot == "砂" {
			shoes = a
		}
	}
	if shoes.SetName != "諧律奇想の断章" {
		t.Errorf("アイコン経由でセット名を補完できていない: %+v", shoes)
	}
}

func TestIsPercentProp(t *testing.T) {
	for _, k := range []string{
		"FIGHT_PROP_CRITICAL", "FIGHT_PROP_CRITICAL_HURT", "FIGHT_PROP_CHARGE_EFFICIENCY",
		"FIGHT_PROP_HEAL_ADD", "FIGHT_PROP_ATTACK_PERCENT", "FIGHT_PROP_ICE_ADD_HURT",
	} {
		if !isPercentProp(k) {
			t.Errorf("%s は割合表示のはず", k)
		}
	}
	for _, k := range []string{"FIGHT_PROP_HP", "FIGHT_PROP_ATTACK", "FIGHT_PROP_ELEMENT_MASTERY", "FIGHT_PROP_BASE_ATTACK"} {
		if isPercentProp(k) {
			t.Errorf("%s は実数のはず", k)
		}
	}
}

func TestTheaterLabel(t *testing.T) {
	if got := theaterLabel(0, 0); got != "" {
		t.Errorf("未挑戦は空にする: %q", got)
	}
	if got := theaterLabel(10, 3); got != "第10幕 (難易度3)" {
		t.Errorf("got %q", got)
	}
	if got := theaterLabel(4, 0); got != "第4幕" {
		t.Errorf("難易度不明のとき: %q", got)
	}
}

// 旅人とドールは元素ごとに別のエントリを持つ。**skillDepotId を見ないと
// 引けない** — 素の avatarId は空オブジェクトになっている。
func TestBuildCharacter_SwitchableElement(t *testing.T) {
	raw := rawAvatar{AvatarID: 10000117, SkillDepotID: 11701}
	c := clientWithMasters(t, "").buildCharacter(context.Background(), raw)

	if c.Name != "ドール（男）" {
		t.Errorf("元素別のエントリを引けていない: %q", c.Name)
	}
	if c.Element != "Fire" {
		t.Errorf("元素: %q", c.Element)
	}
	if c.Icon == "" {
		t.Error("アイコンが引けていない")
	}
}

// skillDepotId が無ければ素の avatarId で引く。**元素別のエントリしか
// 無い相手では何も引けない**が、それは応答に情報が無いということ。
func TestCharacterStore_LookupFallsBack(t *testing.T) {
	c := clientWithMasters(t, "")
	ctx := context.Background()

	// 通常のキャラは skillDepotId が無くても引ける。
	if info, ok := c.chars.Lookup(ctx, 10000002, 0); !ok || info.Element != "Ice" {
		t.Errorf("通常のキャラを引けない: %+v", info)
	}
	// 存在しない depot なら素の id に落ちる (中身は空)。
	if _, ok := c.chars.Lookup(ctx, 10000117, 99999); !ok {
		t.Error("素の avatarId にも落ちていない")
	}
}

// 名刺は .jpg。**拡張子を決め打ちにすると取り違える。**
func TestNamecardKeepsExtension(t *testing.T) {
	c := clientWithMasters(t, "")
	got := nameCardURL(context.Background(), c.namecards, 210042)

	if got != "/api/plugin/genshin/asset/UI_NameCardPic_Ayaka_P.jpg" {
		t.Errorf("名刺の URL: %q", got)
	}
}

// **移行前に保存されたスナップショットは拡張子を持たない。** 補わないと、
// ttl が切れて取り直すまでの間そこだけ画像が出ない (実際に踏んだ)。
func TestAssetFileName(t *testing.T) {
	cases := map[string]string{
		// 古いスナップショット由来 (拡張子なし)
		"UI_AvatarIcon_Venti": "UI_AvatarIcon_Venti.png",
		// 新形式由来 (拡張子あり)。**足すと .jpg.png になって壊れる。**
		"UI_NameCardPic_0_P.jpg":  "UI_NameCardPic_0_P.jpg",
		"UI_AvatarIcon_Ayaka.png": "UI_AvatarIcon_Ayaka.png",
	}
	for in, want := range cases {
		if got := assetFileName(in); got != want {
			t.Errorf("assetFileName(%q) = %q (期待 %q)", in, got, want)
		}
	}
}

// マスター由来のアイコンはパス、応答由来のものは名前だけ。**混在するので
// URL を組む 1 か所で吸収する** — 呼び出し側ごとに剥がすと必ず取りこぼす
// (天賦アイコンで実際に踏んだ)。
func TestAssetURL_StripsPath(t *testing.T) {
	cases := map[string]string{
		// マスター由来 (天賦・命ノ星座・キャラアイコン)
		"/ui/Skill_A_01.png":          "/api/plugin/genshin/asset/Skill_A_01.png",
		"/ui/UI_AvatarIcon_Ayaka.png": "/api/plugin/genshin/asset/UI_AvatarIcon_Ayaka.png",
		// 応答由来 (武器・聖遺物)。拡張子は取得時に補う。
		"UI_EquipIcon_Bow_Dvalin": "/api/plugin/genshin/asset/UI_EquipIcon_Bow_Dvalin",
		// 通さないもの
		"":                  "",
		"/ui/":              "",
		"/ui/../etc/passwd": "",
		"a/b.png":           "",
	}
	for in, want := range cases {
		if got := assetURL(in); got != want {
			t.Errorf("assetURL(%q) = %q (期待 %q)", in, got, want)
		}
	}
}
