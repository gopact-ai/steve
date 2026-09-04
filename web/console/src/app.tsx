import { useEffect, useState } from "react";
import { HashRouter, Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { Activity, BookOpen01, ClipboardCheck, Folder, Inbox01, Moon01, PuzzlePiece01, Server01, Sun, Terminal } from "@untitledui/icons";
import { NavItemBase } from "@/components/application/app-navigation/base-components/nav-item";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
import { useTheme } from "@/providers/theme-provider";
import { BoardPage } from "@/pages/board";
import { ConsolePage } from "@/pages/console";
import { FleetPage } from "@/pages/fleet";
import { HistoryPage } from "@/pages/history";
import { InboxPage } from "@/pages/inbox";
import { ProjectsPage } from "@/pages/projects";
import { SkillsPage } from "@/pages/skills";
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
    const [clock, setClock] = useState(new Date());
    useEffect(() => { const t = window.setInterval(() => setClock(new Date()), 1000); return () => window.clearInterval(t); }, []);

    const running = snap.tasks.filter((t) => t.lane === "running" && !t.parent).length;
    const needsYou = snap.inbox.length;
    const up = snap.nodes.filter((n) => n.up).length;
    const broken = snap.sources.filter((s) => s.wired && s.error).length;
    const groups: { title: string; items: { href: string; label: string; icon: typeof Terminal; badge?: number | string; hot?: boolean }[] }[] = [
        { title: "工作", items: [
            { href: "/console", label: "工作台", icon: Terminal },
            { href: "/tasks", label: "任务", icon: ClipboardCheck, badge: running || undefined },
        ] },
        { title: "环境", items: [
            { href: "/projects", label: "项目", icon: Folder, badge: snap.projects.length || undefined },
            { href: "/fleet", label: "资源", icon: Server01, badge: `${up}/${snap.nodes.length}` },
            { href: "/skills", label: "技能", icon: PuzzlePiece01 },
            { href: "/home", label: "档案", icon: BookOpen01 },
        ] },
        { title: "关注", items: [
            { href: "/inbox", label: "待处理", icon: Inbox01, badge: needsYou || undefined, hot: needsYou > 0 },
        ] },
    ];
    const dark = theme === "dark" || (theme === "system" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);
    return (
        <div className="flex h-screen bg-secondary text-primary">
            <aside className="flex w-64 shrink-0 flex-col border-r border-secondary bg-primary">
                <div className="flex items-center gap-3 px-5 pt-5 pb-3">
                    <div className="flex size-8 items-center justify-center rounded-lg bg-brand-solid text-white"><Terminal className="size-4" /></div>
                    <div>
                        <div className="text-md font-semibold text-primary">steve</div>
                        <div className="text-xs text-tertiary">{snap.hub.node ? `${snap.hub.node} · hub` : "控制台"}</div>
                    </div>
                </div>
                <nav className="flex flex-1 flex-col gap-4 px-4">
                    {groups.map((g) => (
                        <div key={g.title} className="flex flex-col gap-0.5">
                            <div className="px-3 pb-1 text-[11px] font-medium uppercase tracking-wide text-quaternary">{g.title}</div>
                            {g.items.map((item) => (
                                <NavItemBase key={item.href} type="link" href={"#" + item.href} icon={item.icon} current={location.pathname === item.href}
                                    badge={item.badge !== undefined ? <span className={`rounded-full px-2 py-0.5 text-xs ${item.hot ? "bg-warning-primary text-warning-primary" : "bg-secondary text-tertiary"}`}>{item.badge}</span> : undefined}
                                    onClick={(e) => { e.preventDefault(); navigate(item.href); }}>
                                    {item.label}
                                </NavItemBase>
                            ))}
                        </div>
                    ))}
                    <div className="mt-auto flex flex-col gap-0.5 pb-2">
                        <NavItemBase type="link" href="#/history" icon={BookOpen01} current={location.pathname === "/history"} onClick={(e) => { e.preventDefault(); navigate("/history"); }}>历史与审计</NavItemBase>
                    </div>
                </nav>
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
            </aside>
            <main className="min-w-0 flex-1 overflow-auto">
                <Routes>
                    <Route path="/" element={<Navigate to="/console" replace />} />
                    <Route path="/console" element={<ConsolePage />} />
                    <Route path="/tasks" element={<BoardPage />} />
                    <Route path="/plans" element={<Navigate to="/tasks" replace />} />
                    <Route path="/projects" element={<ProjectsPage />} />
                    <Route path="/fleet" element={<FleetPage />} />
                    <Route path="/skills" element={<SkillsPage />} />
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
