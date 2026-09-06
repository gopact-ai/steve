export const when = (t?: string) => (t ? new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "");
export const short = (s?: string, n = 12) => (s ? s.slice(0, n) : "");
export const relative = (t?: string) => {
    if (!t) return "";
    const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000));
    if (s < 60) return `${s}s ago`;
    if (s < 3600) return `${Math.round(s / 60)}m ago`;
    if (s < 86400) return `${Math.round(s / 3600)}h ago`;
    return `${Math.round(s / 86400)}d ago`;
};
