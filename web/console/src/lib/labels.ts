// The words the page uses, and the internal ones they stand for. Internal
// terms show only in tooltips and the audit view.
export const zh = {
    status: { pending: "空闲", running: "执行中", needs_you: "等你处理", set_aside: "已搁置", ended: "已结束" } as Record<string, string>,
    taskState: { draft: "草稿", running: "进行中", idle: "空闲", blocked: "受阻", review: "待审", done: "已完成", failed: "失败", paused: "已暂停", cancelled: "已取消" } as Record<string, string>,
    stepState: { pending: "待执行", ready: "可执行", running: "执行中", verifying: "验证中", "awaiting-human": "等你回答", done: "完成", failed: "失败", skipped: "跳过" } as Record<string, string>,
    repo: { inplace: "直接修改主目录", isolated: "隔离副本，完成后合并" } as Record<string, string>,
    level: { public: "public", internal: "internal", restricted: "restricted", sealed: "sealed" } as Record<string, string>,
    requestType: { disclosure: "允许发送", effect: "外部动作对账", question: "回答问题", pairing: "接入申请" } as Record<string, string>,
    origin: { chat: "对话", plan: "计划", schedule: "定时", delegate: "子任务", repair: "修复" } as Record<string, string>,
};

export const label = (table: Record<string, string>, key?: string) => (key ? table[key] ?? key : "—");

// fmtTokens renders a token count the way people read them: 12.3k, 1.2M.
export function fmtTokens(n?: number): string {
    if (!n) return "0";
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
    if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
    return String(n);
}

// spend says what an attempt cost in the words the data allows: token
// counts when reported, the context size when that is all the adapter
// said, "未上报" when nothing was.
export function spend(t?: { total?: number; context?: number }): string {
    if (t?.total) return fmtTokens(t.total) + " tok";
    if (t?.context) return "上下文 " + fmtTokens(t.context);
    return "未上报";
}

export function fmtSeconds(s?: number): string {
    if (!s) return "0s";
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m${s % 60}s`;
    return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`;
}
