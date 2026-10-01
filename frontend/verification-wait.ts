/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
export const verificationWaitMs = 60_000;

export function verificationWaitSeconds(localNotBefore: number, serverNotBefore: number, now: number): number {
	return Math.max(0, Math.ceil((Math.max(localNotBefore, serverNotBefore) - now) / 1000));
}
