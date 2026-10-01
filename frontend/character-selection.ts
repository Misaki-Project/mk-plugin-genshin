/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import type { BuildCharacter, ShowcaseCharacter } from './api.js';

export function selectedBuild(showcase: ShowcaseCharacter[], characters: BuildCharacter[], index: number | null): BuildCharacter | null {
	if (index == null) return null;
	const icon = showcase[index];
	if (icon == null) return null;
	return characters.find(c => c.avatarId === icon.avatarId) ?? null;
}
