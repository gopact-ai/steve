import { useState } from "react";
import { HashRouter, Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { BookOpen01, ClipboardCheck, Folder, Inbox01, Dataflow03, PuzzlePiece01, Server01, Terminal, ChevronLeftDouble, Menu01, Settings01, Check, X } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { Sheet } from "@/components/steve/drawer";
import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
import { useTheme } from "@/providers/theme-provider";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { ConsolePage } from "@/pages/console";
import { FleetPage } from "@/pages/fleet";
import { HistoryPage } from "@/pages/history";
import { InboxPage } from "@/pages/inbox";
import { ProjectsPage } from "@/pages/projects";
import { SkillsPage } from "@/pages/skills";
import { MCPPage } from "@/pages/mcp";
import { HomePage } from "@/pages/home";
import { ReviewProvider } from "@/components/steve/review-context";

export function App() {
    const navigate = useNavigate();
    return <FleetProvider><IntentProvider onNavigate={() => navigate("/console")}><ReviewProvider><Shell /></ReviewProvider></IntentProvider></FleetProvider>;
}

function Shell() {
    const { snap, live } = useFleet();
    const location = useLocation();
    const { theme, setTheme } = useTheme();
    const desktop = useBreakpoint("xl");
    const tablet = useBreakpoint("sm");
    const [navCollapsed, setNavCollapsed] = useState(() => { try { return localStorage.getItem("steve.nav.collapsed") === "1"; } catch { return false; } });
    const [mobileNav, setMobileNav] = useState(false);
    const compact = navCollapsed || !desktop;
    const running = snap.tasks.filter((t) => t.execution === "running" && !t.parent).length;
    const up = snap.nodes.filter((n) => n.up).length;
    const broken = snap.sources.filter((s) => s.wired && s.error).length;
    const groups = [
        { title: "工作", items: [
            { href: "/console", label: "工作台", icon: Terminal, badge: 0 },
            { href: "/console?view=board", label: "任务", icon: ClipboardCheck, badge: running },
            { href: "/inbox", label: "待处理", icon: Inbox01, badge: snap.inbox.length },
        ] },
        { title: "管理", items: [
            { href: "/projects", label: "项目", icon: Folder, badge: 0 },
            { href: "/fleet", label: "资源", icon: Server01, badge: 0 },
            { href: "/skills", label: "技能", icon: PuzzlePiece01, badge: 0 },
            { href: "/mcp", label: "MCP", icon: Dataflow03, badge: 0 },
            { href: "/home", label: "档案", icon: BookOpen01, badge: 0 },
        ] },
    ];
    const selected = location.pathname + location.search;
    const connection = live === "live" ? (broken ? "部分数据不可用" : "已连接") : live === "unauthorized" ? "需要认证" : "正在连接";
    const navigation = (small: boolean) => <>
        <div className="app-brand">
            <span className="app-mark" aria-hidden="true"><Terminal /></span>
            {!small && <span><strong>Steve</strong><small>工作空间</small></span>}
        </div>
        <nav aria-label="主导航" className="app-navigation">
            {groups.map((group) => <div key={group.title} className="app-nav-group">
                {!small && <span className="app-nav-label">{group.title}</span>}
                {group.items.map((item) => <a key={item.href} href={"#" + item.href} aria-label={small ? item.label : undefined}
                    aria-current={(item.href === "/home" ? location.pathname === "/home" : selected === item.href) ? "page" : undefined} title={small ? item.label : undefined}
                    className="app-nav-item" onClick={() => setMobileNav(false)}>
                    <item.icon aria-hidden="true" />
                    {!small && <span>{item.label}</span>}
                    {!!item.badge && <span className={small ? "app-nav-dot" : "app-nav-count"}>{small ? null : item.badge}</span>}
                </a>)}
            </div>)}
            <div className="app-nav-bottom">
                <a href="#/history" aria-label={small ? "历史与审计" : undefined} title={small ? "历史与审计" : undefined} aria-current={location.pathname === "/history" ? "page" : undefined} className="app-nav-item" onClick={() => setMobileNav(false)}><BookOpen01 aria-hidden="true" />{!small && <span>历史与审计</span>}</a>
            </div>
        </nav>
        <div className="app-sidebar-footer">
            {small && <span role="status" className="app-compact-status" aria-label={connection} title={`${connection} · ${up}/${snap.nodes.length} 台设备在线`}><span className={`connection-dot ${live === "live" && !broken ? "connected" : ""}`} /></span>}
            {!small && <div className="app-connection" title={`hub ${snap.hub.version || ""} · ${up}/${snap.nodes.length} 台设备在线`}>
                <span className={`connection-dot ${live === "live" && !broken ? "connected" : ""}`} />
                <span>{connection}</span><span className="app-device-count">{up} 台在线</span>
            </div>}
            <div className="app-sidebar-tools">
                <Dropdown.Root>
                    <AriaButton className="workbench-icon-button" aria-label="外观"><Settings01 aria-hidden="true" /></AriaButton>
                    <Dropdown.Popover placement="top start" className="w-48"><Dropdown.Menu aria-label="外观" onAction={(key) => setTheme(key as "light" | "dark" | "system")}>
                        <Dropdown.Section><Dropdown.SectionHeader className="px-3 py-1 text-xs text-tertiary">外观</Dropdown.SectionHeader>
                            {([['system', '跟随系统'], ['light', '浅色'], ['dark', '深色']] as const).map(([key, label]) => <Dropdown.Item key={key} id={key} label={label} icon={theme === key ? Check : undefined} />)}
                        </Dropdown.Section>
                    </Dropdown.Menu></Dropdown.Popover>
                </Dropdown.Root>
                {!small && <span className="app-version" title={snap.hub.node}>hub {snap.hub.version || "—"}</span>}
                {desktop && <button type="button" className="workbench-icon-button app-collapse" aria-label={small ? "展开菜单" : "收起菜单"} title={small ? "展开菜单" : "收起菜单"} onClick={() => { const next = !navCollapsed; setNavCollapsed(next); try { localStorage.setItem("steve.nav.collapsed", next ? "1" : "0"); } catch { /* Preference is optional. */ } }}><ChevronLeftDouble className={small ? "rotate-180" : ""} aria-hidden="true" /></button>}
            </div>
        </div>
    </>;
    return <div className="workbench-shell">
        <a className="skip-link" href="#main-content" onClick={(event) => { event.preventDefault(); document.getElementById("main-content")?.focus(); }}>跳到内容</a>
        {tablet && <aside className={`app-sidebar ${compact ? "is-compact" : ""}`}>{navigation(compact)}</aside>}
        {!tablet && <div className="app-mobile-bar"><button type="button" className="workbench-icon-button" aria-label="导航菜单" onClick={() => setMobileNav(true)}><Menu01 aria-hidden="true" /></button><strong>Steve</strong><span className={`connection-dot ${live === "live" ? "connected" : ""}`} title={connection} /></div>}
        {mobileNav && !tablet && <Sheet label="导航菜单" side="left" width={260} onClose={() => setMobileNav(false)}><button type="button" className="sheet-close workbench-icon-button" aria-label="关闭导航菜单" onClick={() => setMobileNav(false)}><X aria-hidden="true" /></button><div className="app-sidebar is-mobile">{navigation(false)}</div></Sheet>}
        <main id="main-content" tabIndex={-1} className="app-main">
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
    </div>;
}

export default function AppWithRouter() { return <HashRouter><App /></HashRouter>; }
