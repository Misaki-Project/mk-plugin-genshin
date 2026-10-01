<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->
<template>
<MkFolder>
	<template #label>原神</template>
	<template #suffix>{{ uids.length }} / {{ limit }} 件</template>
	<div class="_gaps_m">
		<div v-for="uid in uids" :key="uid" class="_buttons">
			<span>UID {{ uid }}</span>
			<MkButton :disabled="busy" @click="unlink(uid)">連携を解除</MkButton>
		</div>
		<MkInput v-model="draft" type="text" :disabled="busy" placeholder="800000000">
			<template #label>連携する原神UID</template>
			<template #caption>本人確認が完了したUIDだけをプロフィールに表示します。リモートサーバーの連携情報は上限に含みません。</template>
		</MkInput>
		<MkButton primary :disabled="busy || uids.length >= limit" @click="begin">紐づけコードを発行</MkButton>
		<div v-if="pending" class="_gaps_s">
			<div>確認対象: {{ pending.uid }}</div>
			<MkInput :modelValue="pending.code" readonly>
				<template #label>紐づけコード</template>
				<template #caption>数字6桁と間の記号を省略・変更せず、そのままステータスメッセージに追加してください。</template>
			</MkInput>
			<div>原神のステータスメッセージに上のコードを追加して保存し、一度ゲームからログアウトしてから「認証する」を押してください。</div>
			<div>反映には時間がかかる場合があります。コードは発行から10分間有効です。</div>
			<div>認証確認の送信後は、最低60秒待ってから再確認してください。Enkaの反映待ちが長い場合は、その期限まで待機します。</div>
			<div role="timer">残り {{ remaining }} 秒</div>
			<div v-if="waitSeconds > 0">反映待ちです。{{ waitSeconds }} 秒後に再確認できます。</div>
			<MkButton primary :disabled="busy || remaining === 0 || waitSeconds > 0 || pending.attempts >= 10" @click="verify">認証する</MkButton>
			<div v-if="remaining === 0">コードの有効期限が切れました。再発行してください。</div>
			<div v-else-if="pending.attempts >= 10">確認回数の上限に達しました。コードを再発行してください。</div>
		</div>
		<div v-if="message" role="status">{{ message }}</div>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import { MkInput, MkButton, MkFolder } from '@/plugin-api.js';
import { api } from './api.js';
import type { LinkChallenge, MeResponse, VerifyResponse } from './api.js';
import { verificationWaitMs, verificationWaitSeconds } from './verification-wait.js';

const uids = ref<string[]>([]);
const limit = ref(1);
const draft = ref('');
const pending = ref<LinkChallenge | null>(null);
const busy = ref(false);
const message = ref('');
const now = ref(Date.now());
const verifyNotBefore = ref(0);
const remaining = computed(() => Math.max(0, Math.ceil(((pending.value ? Date.parse(pending.value.expiresAt) : 0) - now.value) / 1000)));
const waitSeconds = computed(() => verificationWaitSeconds(verifyNotBefore.value, pending.value ? Date.parse(pending.value.nextCheckAt) : 0, now.value));
let timer: number | undefined;

async function reload(): Promise<void> {
	const me = await api<MeResponse>('me');
	uids.value = me.uids;
	limit.value = me.limit;
	pending.value = me.pending;
}

async function run(action: () => Promise<void>): Promise<void> {
	busy.value = true;
	message.value = '';
	try { await action(); } catch (err) {
		message.value = (err as { message?: string } | null)?.message ?? '処理に失敗しました。しばらく待って再確認してください。';
		try { await reload(); } catch { /* Preserve the current UI on temporary network failure. */ }
	} finally { busy.value = false; }
}

async function begin(): Promise<void> {
	await run(async () => {
		pending.value = await api<LinkChallenge>('me/begin', { uid: draft.value.trim() });
		now.value = Date.now();
	});
}

async function verify(): Promise<void> {
	if (pending.value == null || busy.value || waitSeconds.value > 0) return;
	const code = pending.value.code;
	now.value = Date.now();
	verifyNotBefore.value = now.value + verificationWaitMs;
	await run(async () => {
		const res = await api<VerifyResponse>('me/verify', { code });
		await reload();
		message.value = res.verified ? '連携が完了しました。ゲーム内の紐づけコードは削除できます。' : 'まだコードを確認できません。保存後にゲームからログアウトしたことを確認し、反映を待ってください。';
	});
}

async function unlink(uid: string): Promise<void> {
	await run(async () => { await api('me/unlink', { uid }); await reload(); message.value = '連携を解除しました。'; });
}

onMounted(async () => {
	timer = window.setInterval(() => { now.value = Date.now(); }, 1000);
	await run(reload);
});
onUnmounted(() => { if (timer != null) window.clearInterval(timer); });
</script>
