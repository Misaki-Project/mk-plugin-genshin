/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import { expect, it } from 'vitest';
import { verificationWaitMs, verificationWaitSeconds } from './verification-wait.js';

it('requires 60 seconds locally, including while a response is pending', () => {
	const sent = 100_000;
	const next = sent + verificationWaitMs;
	expect(verificationWaitSeconds(next, 0, sent)).toBe(60);
	expect(verificationWaitSeconds(next, 0, sent + 59_999)).toBe(1);
	expect(verificationWaitSeconds(next, 0, sent + 60_000)).toBe(0);
});

it('respects a later server cooldown or Enka TTL without extending the challenge', () => {
	const sent = 100_000;
	expect(verificationWaitSeconds(sent + verificationWaitMs, sent + 300_000, sent)).toBe(300);
	expect(verificationWaitSeconds(sent + verificationWaitMs, sent + 300_000, sent + 300_000)).toBe(0);
	expect(verificationWaitSeconds(0, sent + verificationWaitMs, sent)).toBe(60);
});
