import type { Translator } from "./i18n";
import type { SSHAutoStart, SSHRestartState } from "./api/ssh";

export type AutoStartTone = "quiet" | "info" | "warn" | "bad";
export interface AutoStartLine { tone: AutoStartTone; title: string; detail?: string; hint?: string }

export const restartOffered = (_role: string | undefined, _state: SSHRestartState | null | undefined) => false;
export const restartConfirms = (_up: boolean) => false;
export const restartHiddenBy = (_status: number) => false;
export const restartRunning = (_state: SSHRestartState | null | undefined) => false;
export const restartPollDelay = (_state: SSHRestartState | null | undefined, _posting: boolean) => 0;
export function autoStartLine(_auto: SSHAutoStart | undefined, _tr: Translator, _time: (at: string) => string): AutoStartLine | null { return null; }
