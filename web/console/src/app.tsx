import { useEffect, useState } from "react";
import { HashRouter, Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { Activity, BookOpen01, ClipboardCheck, Folder, Inbox01, Moon01, Dataflow03, PuzzlePiece01, Server01, Sun, Terminal, ChevronLeftDouble, ChevronRightDouble } from "@untitledui/icons";
import { NavItemBase } from "@/components/application/app-navigation/base-components/nav-item";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
import { useTheme } from "@/providers/theme-provider";
import { ConsolePage } from "@/pages/console";
import { FleetPage } from "@/pages/fleet";
import { HistoryPage } from "@/pages/history";
import { InboxPage } from "@/pages/inbox";
import { ProjectsPage } from "@/pages/projects";
import { SkillsPage } from "@/pages/skills";
import { MCPPage } from "@/pages/mcp";
import { HomePage } from "@/pages/home";

export function App() {
    const navigate = useNavigate();
    return (
        <FleetProvider>
            <IntentProvider onNavigate={() => navigate("/console")}>
                <Shell />
            </IntentProvider>
        </FleetProvider>
    );
}

function Shell() {
    const { snap, live } = useFleet();
    const location = useLocation();
    const navigate = useNavigate();
    const { theme, setTheme } = useTheme();
    const [navCollapsed, setNavCollapsedState] = useState<boolean>(() => { try { return localStorage.getItem("steve.nav.collapsed") === "1"; } catch { return false; } });
    const setNavCollapsed = (v: boolean) => { setNavCollapsedState(v); try { localStorage.setItem("steve.nav.collapsed", v ? "1" : "0"); } catch { /* ignore */ } };
    const [clock, setClock] = useState(new Date());
    useEffect(() => { const t = window.setInterval(() => setClock(new Date()), 1000); return () => window.clearInterval(t); }, []);

    const running = snap.tasks.filter((t) => t.lane === "running" && !t.parent).length;
    const needsYou = snap.inbox.length;
    const up = snap.nodes.filter((n) => n.up).length;
    const broken = snap.sources.filter((s) => s.wired && s.error).length;
    const groups: { title: string; items: { href: string; label: string; icon: typeof Terminal; badge?: number | string; hot?: boolean }[] }[] = [
        { title: "工作", items: [
            { href: "/console", label: "工作台", icon: Terminal },
            { href: "/console?view=board", label: "任务", icon: ClipboardCheck, badge: running || undefined },
        ] },
        { title: "环境", items: [
            { href: "/projects", label: "项目", icon: Folder, badge: snap.projects.length || undefined },
            { href: "/fleet", label: "资源", icon: Server01, badge: `${up}/${snap.nodes.length}` },
            { href: "/skills", label: "技能", icon: PuzzlePiece01 },
            { href: "/mcp", label: "MCP", icon: Dataflow03 },
            { href: "/home", label: "档案", icon: BookOpen01 },
        ] },
        { title: "关注", items: [
            { href: "/inbox", label: "待处理", icon: Inbox01, badge: needsYou || undefined, hot: needsYou > 0 },
        ] },
    ];
    const dark = theme === "dark" || (theme === "system" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);
    const compact = (item: { href: string; label: string; icon: typeof Terminal; badge?: number | string; hot?: boolean }) => (
        <a key={item.href} href={"#" + item.href} title={item.label} aria-label={item.label} onClick={(e) => { e.preventDefault(); navigate(item.href); }}
            className={`relative flex size-9 items-center justify-center rounded-md transition ${(location.pathname + location.search === item.href || (item.href === "/console" && location.pathname === "/console" && !location.search)) ? "bg-active text-fg-brand-primary" : "text-fg-quaternary hover:bg-primary_hover hover:text-fg-quaternary_hover"}`}>
            <item.icon className="size-5" />
            {item.badge !== undefined && <span className={`absolute right-0.5 top-0.5 size-2 rounded-full ${item.hot ? "bg-warning-solid" : "bg-quaternary"}`} />}
        </a>
    );
    return (
        <div className="flex h-screen bg-secondary text-primary">
            <aside className={`flex shrink-0 flex-col border-r border-secondary bg-primary transition-[width] ${navCollapsed ? "w-14" : "w-64"}`}>
                <div className={`flex items-center gap-3 pt-5 pb-3 ${navCollapsed ? "justify-center px-0" : "px-5"}`}>
                    <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-brand-solid text-white" title={snap.hub.node ? `${snap.hub.node} · hub` : "控制台"}><Terminal className="size-4" /></div>
                    {!navCollapsed && (
                        <div className="min-w-0">
                            <div className="text-md font-semibold text-primary">steve</div>
                            <div className="truncate text-xs text-tertiary">{snap.hub.node ? `${snap.hub.node} · hub` : "控制台"}</div>
                        </div>
                    )}
                </div>
                {navCollapsed ? (
                    <nav className="flex flex-1 flex-col items-center gap-1 px-2">
                        {groups.flatMap((g) => g.items).map(compact)}
                        <div className="mt-auto flex flex-col items-center gap-1 pb-2">
                            {compact({ href: "/history", label: "历史与审计", icon: BookOpen01 })}
                            <button type="button" onClick={() => setNavCollapsed(false)} title="展开菜单" aria-label="展开菜单" className="flex size-9 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary_hover hover:text-fg-quaternary_hover"><ChevronRightDouble className="size-4" /></button>
                        </div>
                    </nav>
                ) : (
                    <nav className="flex flex-1 flex-col gap-4 px-4">
                        {groups.map((g) => (
                            <div key={g.title} className="flex flex-col gap-0.5">
                                <div className="px-3 pb-1 u-label">{g.title}</div>
                                {g.items.map((item) => (
                                    <NavItemBase key={item.href} type="link" href={"#" + item.href} icon={item.icon} current={location.pathname + location.search === item.href || (item.href === "/console" && location.pathname === "/console" && !location.search)}
                                        badge={item.badge !== undefined ? <span className={`rounded-full px-2 py-0.5 text-xs ${item.hot ? "bg-warning-primary text-warning-primary" : "bg-secondary text-tertiary"}`}>{item.badge}</span> : undefined}
                                        onClick={(e) => { e.preventDefault(); navigate(item.href); }}>
                                        {item.label}
                                    </NavItemBase>
                                ))}
                            </div>
                        ))}
                        <div className="mt-auto flex flex-col gap-0.5 pb-2">
                            <NavItemBase type="link" href="#/history" icon={BookOpen01} current={location.pathname === "/history"} onClick={(e) => { e.preventDefault(); navigate("/history"); }}>历史与审计</NavItemBase>
                            <button type="button" onClick={() => setNavCollapsed(true)} className="flex items-center gap-2 rounded-md px-3 py-1.5 text-xs text-quaternary hover:bg-primary_hover hover:text-primary" title="收起菜单"><ChevronLeftDouble className="size-4" />收起</button>
                        </div>
                    </nav>
                )}
                {navCollapsed ? (
                    <div className="flex flex-col items-center gap-2 border-t border-secondary py-3">
                        <span className={`size-2 rounded-full ${live === "live" ? "bg-success-solid" : "bg-warning-solid"}`} title={live === "live" ? "实时" : "重连中"} />
                        <ButtonUtility size="xs" color="tertiary" tooltip={dark ? "浅色" : "深色"} icon={dark ? Sun : Moon01} onClick={() => setTheme(dark ? "light" : "dark")} />
                    </div>
                ) : (
                    <div className="border-t border-secondary px-5 py-3 text-xs text-tertiary">
                        <div className="flex items-center justify-between">
                            <span className="flex items-center gap-1.5">
                                <span className={`size-2 rounded-full ${live === "live" ? "bg-success-solid" : "bg-warning-solid"}`} />
                                {live === "live" ? "实时" : "重连中"}
                            </span>
                            <span>{clock.toLocaleTimeString()}</span>
                        </div>
                        <div className="mt-1 flex items-center gap-1.5"><Activity className="size-3" />{running} 在跑 · {snap.tasks.length} 任务 · {snap.plans.length} 计划</div>
                        <div className="mt-1 flex items-center justify-between">
                            <span title={snap.sources.map((s) => `${s.name}: ${s.wired ? (s.error || "ok") : "未接线"}`).join("\n")}>
                                {broken ? <span className="text-error-primary">{broken} 个数据源读取失败</span> : `hub ${snap.hub.version || ""}`}
                            </span>
                            <ButtonUtility size="xs" color="tertiary" tooltip={dark ? "浅色" : "深色"} icon={dark ? Sun : Moon01} onClick={() => setTheme(dark ? "light" : "dark")} />
                        </div>
                    </div>
                )}
            </aside>
            <main className="min-w-0 flex-1 overflow-auto">
                <Routes>
                    <Route path="/" element={<Navigate to="/console" replace />} />
                    <Route path="/console" element={<ConsolePage />} />
                    <Route path="/tasks" element={<Navigate to="/console?view=board" replace />} />
                    <Route path="/plans" element={<Navigate to="/tasks" replace />} />
                    <Route path="/projects" element={<ProjectsPage />} />
                    <Route path="/fleet" element={<FleetPage />} />
                    <Route path="/skills" element={<SkillsPage />} />
                    <Route path="/mcp" element={<MCPPage />} />
                    <Route path="/home" element={<HomePage />} />
                    <Route path="/inbox" element={<InboxPage />} />
                    <Route path="/ledger" element={<Navigate to="/inbox" replace />} />
                    <Route path="/history" element={<HistoryPage />} />
                    <Route path="/activity" element={<Navigate to="/history" replace />} />
                </Routes>
            </main>
        </div>
    );
}

export default function AppWithRouter() {
    return (
        <HashRouter>
            <App />
        </HashRouter>
    );
}
