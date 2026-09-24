import type { mcpToolsZh } from "../zh/mcpTools.ts";

export const mcpToolsEn = {
    "mcpTools.noSummary": "No description",
    "mcpTools.fullDescription": "Full description",
    "mcpTools.noDescription": "This tool has no description.",
    "mcpTools.inputParameters": "Input parameters"
} as const satisfies Record<keyof typeof mcpToolsZh, string>;
