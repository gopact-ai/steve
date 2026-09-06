export const mcpToolsZh = {
    "mcpTools.noSummary": "暂无说明",
    "mcpTools.fullDescription": "完整说明",
    "mcpTools.noDescription": "此工具未提供说明。",
    "mcpTools.inputParameters": "输入参数"
} as const;

export const mcpToolsEn = {
    "mcpTools.noSummary": "No description",
    "mcpTools.fullDescription": "Full description",
    "mcpTools.noDescription": "This tool has no description.",
    "mcpTools.inputParameters": "Input parameters"
} as const satisfies Record<keyof typeof mcpToolsZh, string>;
