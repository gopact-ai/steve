import {selectionZh,selectionEn} from "./selection.ts";
import {sideChatZh,sideChatEn} from "./side-chat.ts";
import { consoleEn, consoleZh } from "./console.ts";
import { domainEn as consoleChromeEn, domainZh as consoleChromeZh } from "./console-chrome.ts";
import { materialsEn, materialsZh } from "./materials.ts";
import { settingsPageEn, settingsPageZh } from "./settings-page.ts";
import { mcpToolsEn, mcpToolsZh } from "./mcpTools.ts";
import { tasksEn, tasksZh } from "./tasks.ts";
import { settingsEditorEn, settingsEditorZh } from "./settingsEditor.ts";
import { inboxEn, inboxZh } from "./inbox.ts";
import { historyEn, historyZh } from "./history.ts";
import { boardEn, boardZh } from "./board.ts";
import { homeEn, homeZh } from "./home.ts";
import { skillsEn, skillsZh } from "./skills.ts";
import { mcpEn, mcpZh } from "./mcp.ts";
import { fleetEn, fleetZh } from "./fleet.ts";
import { projectsEn, projectsZh } from "./projects.ts";
import { commonEn, commonZh } from "./common.ts";
import { usageEn, usageZh } from "./usage.ts";

export const catalogs = {
    zh: { ...selectionZh, ...sideChatZh, ...commonZh, ...consoleZh, ...consoleChromeZh, ...materialsZh, ...settingsPageZh, ...mcpToolsZh, ...tasksZh, ...settingsEditorZh, ...inboxZh, ...historyZh, ...boardZh, ...homeZh, ...skillsZh, ...mcpZh, ...fleetZh, ...projectsZh, ...usageZh },
    en: { ...selectionEn, ...sideChatEn, ...commonEn, ...consoleEn, ...consoleChromeEn, ...materialsEn, ...settingsPageEn, ...mcpToolsEn, ...tasksEn, ...settingsEditorEn, ...inboxEn, ...historyEn, ...boardEn, ...homeEn, ...skillsEn, ...mcpEn, ...fleetEn, ...projectsEn, ...usageEn },
} as const;
