<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="data?.linked" :class="$style.card">
	<!--
		名刺画像を背景に敷く。引けなかった場合は無地のままにする
		(画像が出ないだけで表示そのものは壊さない)。
	-->
	<div v-if="data.nameCard" :class="$style.bg" :style="{ backgroundImage: `url('${data.nameCard}')` }"></div>
	<div :class="$style.scrim"></div>

	<div :class="$style.body">
		<div :class="$style.head">
			<img v-if="data.profileIcon" :class="$style.avatar" :src="data.profileIcon" alt=""/>
			<div :class="$style.names">
				<div :class="$style.nickname">{{ data.nickname }}</div>
				<div v-if="data.signature" :class="$style.signature">{{ data.signature }}</div>
			</div>
		</div>

		<div :class="$style.stats">
			<span :class="$style.stat"><b>AR {{ data.adventureRank }}</b></span>
			<span :class="$style.stat">世界ランク {{ data.worldLevel }}</span>
			<span v-if="data.spiral" :class="$style.stat">
				深境螺旋 {{ data.spiral }}<template v-if="data.spiralStars > 0"> ★{{ data.spiralStars }}</template>
			</span>
			<span v-if="data.theater" :class="$style.stat">
				{{ data.theater }}<template v-if="data.theaterStars > 0"> ★{{ data.theaterStars }}</template>
			</span>
			<span v-if="data.achievements > 0" :class="$style.stat">実績 {{ data.achievements }}</span>
			<span v-if="data.fetterCount > 0" :class="$style.stat">好感度Lv10 {{ data.fetterCount }}</span>
			<span v-if="data.region" :class="$style.stat">{{ data.region }}</span>
		</div>

		<div v-if="showcase.length > 0" :class="$style.showcase">
			<button
				v-for="(c, i) in showcase"
				:key="i"
				type="button"
				class="_button"
				:class="[$style.chara, { [$style.charaActive]: selected === i }]"
				@click="toggle(i)"
			>
				<img v-if="c.icon" :class="$style.charaIcon" :src="c.icon" alt=""/>
				<span :class="$style.charaLv">Lv.{{ c.level }}</span>
			</button>
		</div>

		<!--
			選んだキャラのビルド。ショーケースに飾っていても詳細を公開して
			いなければ build は無いので、その場合は何も開かない。
		-->
		<div v-if="build" :class="$style.build">
			<div :class="$style.buildHead">
				<span :class="$style.buildName">{{ build.name || `#${build.avatarId}` }}</span>
				<span :class="$style.buildMeta">Lv.{{ build.level }}</span>
				<span :class="$style.buildMeta">命ノ星座 {{ build.constellation }}</span>
				<span v-if="build.friendship > 0" :class="$style.buildMeta">好感度 {{ build.friendship }}</span>
			</div>

			<div v-if="build.talents.length > 0" :class="$style.talents">
				<span v-for="(t, i) in build.talents" :key="i" :class="$style.talent">
					<img v-if="t.icon" :class="$style.talentIcon" :src="t.icon" alt=""/>
					<b>{{ t.level }}</b><span v-if="t.extra > 0" :class="$style.talentExtra">+{{ t.extra }}</span>
				</span>
			</div>

			<div v-if="build.weapon" :class="$style.weapon">
				<img v-if="build.weapon.icon" :class="$style.weaponIcon" :src="build.weapon.icon" alt=""/>
				<div :class="$style.weaponBody">
					<div>
						<span :class="$style.weaponName">{{ build.weapon.name }}</span>
						<span :class="$style.buildMeta">Lv.{{ build.weapon.level }} / R{{ build.weapon.refine }}</span>
					</div>
					<div :class="$style.statLine">
						<span v-for="(st, i) in build.weapon.stats" :key="i">{{ st.label }} {{ fmt(st) }}</span>
					</div>
				</div>
			</div>

			<div v-if="build.stats.length > 0" :class="$style.statGrid">
				<div v-for="(st, i) in build.stats" :key="i" :class="$style.statRow">
					<span :class="$style.statLabel">{{ st.label }}</span>
					<span :class="$style.statValue">{{ fmt(st) }}</span>
				</div>
			</div>

			<div v-if="build.artifacts.length > 0" :class="$style.artifacts">
				<div v-for="(a, i) in build.artifacts" :key="i" :class="$style.artifact">
					<img v-if="a.icon" :class="$style.artifactIcon" :src="a.icon" alt=""/>
					<div :class="$style.artifactBody">
						<div :class="$style.artifactHead">
							<span :class="$style.artifactSlot">{{ a.slot }}</span>
							<span :class="$style.artifactMain">{{ a.main.label }} {{ fmt(a.main) }}</span>
							<span :class="$style.buildMeta">+{{ a.level }}</span>
						</div>
						<div :class="$style.artifactSet">{{ a.setName }}</div>
						<div :class="$style.statLine">
							<span v-for="(st, j) in a.subs" :key="j">{{ st.label }} {{ fmt(st) }}</span>
						</div>
					</div>
				</div>
			</div>
		</div>

		<div :class="$style.footer">UID {{ data.uid }}</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { type SlotContext } from '@/plugin-api.js';
import { api } from './api.js';
import type { ProfileResponse, LinkedProfile, Stat } from './api.js';

const props = defineProps<{ ctx: SlotContext }>();

const data = ref<LinkedProfile | null>(null);
const selected = ref<number | null>(null);

// 表示に使うのはショーケースの並び。ビルド詳細 (characters) は非公開だと
// 空になるので、アイコン列は従来どおり showcase から作る。
const showcase = computed(() => data.value?.showcase ?? []);

const build = computed(() => {
	if (data.value == null || selected.value == null) return null;
	const icon = showcase.value[selected.value];
	if (icon == null) return null;
	// showcase と characters は同じ並びで返るが、詳細が非公開だと
	// characters 側だけ短くなる。位置ではなく件数で照合する。
	return data.value.characters[selected.value] ?? null;
});

function toggle(i: number): void {
	selected.value = selected.value === i ? null : i;
}

/** 会心率のような割合は "%" を付ける。整数なら小数点を出さない。 */
function fmt(st: Stat): string {
	const v = Number.isInteger(st.value) ? String(st.value) : st.value.toFixed(1);
	return st.percent ? `${v}%` : v;
}

onMounted(async () => {
	const user = props.ctx.user;
	if (user == null) return;
	// リモートユーザーには出さない。このインスタンスに登録された UID しか
	// 持っていないので、他所のユーザーでは必ず未登録になる。
	if (user.host != null) return;

	try {
		const res = await api<ProfileResponse>('profile', { userId: user.id });
		if (res.linked) data.value = res;
	} catch (err) {
		// 表示できないだけで済ませる。原神のデータが取れないせいで
		// プロフィール全体が壊れてはいけない。
		console.error('[plugin:genshin] プロフィールの取得に失敗しました', err);
	}
});
</script>

<style lang="scss" module>
.card {
	position: relative;
	margin: 8px 0;
	border-radius: var(--MI-radius, 12px);
	overflow: hidden;
	background: var(--MI_THEME-panel);
	border: 1px solid var(--MI_THEME-divider);
}

.bg {
	position: absolute;
	inset: 0;
	background-size: cover;
	background-position: center;
	opacity: 0.35;
}

/* 背景の上でも文字が読めるようにする。名刺は明暗が一定でない。 */
.scrim {
	position: absolute;
	inset: 0;
	background: linear-gradient(to right, var(--MI_THEME-panel) 20%, transparent);
}

.body {
	position: relative;
	padding: 12px 14px;
}

.head {
	display: flex;
	align-items: center;
	gap: 10px;
}

.avatar {
	width: 44px;
	height: 44px;
	border-radius: 100%;
	background: var(--MI_THEME-bg);
	flex-shrink: 0;
}

.names {
	min-width: 0;
}

.nickname {
	font-weight: 700;
	line-height: 1.2;
}

.signature {
	font-size: 0.85em;
	opacity: 0.75;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.stats {
	display: flex;
	flex-wrap: wrap;
	gap: 6px 10px;
	margin-top: 10px;
	font-size: 0.9em;
}

.stat {
	opacity: 0.9;
}

.showcase {
	display: flex;
	gap: 8px;
	margin-top: 10px;
	flex-wrap: wrap;
}

.chara {
	display: flex;
	flex-direction: column;
	align-items: center;
	width: 46px;
	padding: 2px 0;
	border-radius: 8px;
	/* 押せることが分かるように、選択中は枠を出す。 */
	border: 2px solid transparent;
}

.charaActive {
	border-color: var(--MI_THEME-accent);
	background: var(--MI_THEME-buttonBg);
}

.charaIcon {
	width: 42px;
	height: 42px;
	border-radius: 8px;
	background: var(--MI_THEME-bg);
}

.charaLv {
	font-size: 0.72em;
	opacity: 0.8;
}

.build {
	margin-top: 10px;
	padding: 10px;
	border-radius: 8px;
	background: var(--MI_THEME-bg);
	font-size: 0.85em;
}

.buildHead {
	display: flex;
	flex-wrap: wrap;
	align-items: baseline;
	gap: 4px 8px;
}

.buildName {
	font-weight: 700;
}

.buildMeta {
	opacity: 0.7;
	font-size: 0.9em;
}

.talents {
	display: flex;
	gap: 10px;
	margin-top: 8px;
}

.talent {
	display: flex;
	align-items: center;
	gap: 3px;
}

.talentIcon {
	width: 20px;
	height: 20px;
}

.talentExtra {
	color: var(--MI_THEME-accent);
	font-size: 0.85em;
}

.weapon {
	display: flex;
	gap: 8px;
	margin-top: 8px;
	align-items: center;
}

.weaponIcon {
	width: 34px;
	height: 34px;
	border-radius: 6px;
	background: var(--MI_THEME-panel);
	flex-shrink: 0;
}

.weaponBody {
	min-width: 0;
}

.weaponName {
	font-weight: 600;
	margin-right: 6px;
}

.statGrid {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
	gap: 2px 12px;
	margin-top: 8px;
}

.statRow {
	display: flex;
	justify-content: space-between;
	gap: 8px;
}

.statLabel {
	opacity: 0.7;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.statValue {
	font-variant-numeric: tabular-nums;
	flex-shrink: 0;
}

.statLine {
	display: flex;
	flex-wrap: wrap;
	gap: 2px 10px;
	opacity: 0.8;
	font-size: 0.9em;
}

.artifacts {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
	gap: 8px;
	margin-top: 10px;
}

.artifact {
	display: flex;
	gap: 8px;
}

.artifactIcon {
	width: 34px;
	height: 34px;
	border-radius: 6px;
	background: var(--MI_THEME-panel);
	flex-shrink: 0;
}

.artifactBody {
	min-width: 0;
	flex: 1;
}

.artifactHead {
	display: flex;
	align-items: baseline;
	gap: 6px;
}

.artifactSlot {
	opacity: 0.7;
	flex-shrink: 0;
}

.artifactMain {
	font-weight: 600;
}

.artifactSet {
	font-size: 0.85em;
	opacity: 0.6;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.footer {
	margin-top: 10px;
	font-size: 0.75em;
	opacity: 0.55;
}
</style>
