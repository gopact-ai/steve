import { CoordinationProvider, useCoordination } from "@/lib/coordination";
import { DesktopOnboarding } from "@/components/steve/desktop-onboarding";
import { SelectionProvider } from "@/providers/selection-provider";
import { SideChatProvider } from "@/providers/side-chat-provider";
import { useState } from "react";
import { HashRouter, Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { BookOpen01, ClipboardCheck, Folder, Inbox01, Dataflow03, PuzzlePiece01, Server01, Terminal, ChevronLeftDouble, Menu01, Settings01, X } from "@untitledui/icons";
import { Sheet } from "@/components/steve/drawer";
import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { ConsolePage } from "@/pages/console";
import { FleetPage } from "@/pages/fleet";
import { HistoryPage } from "@/pages/history";
import { InboxPage } from "@/pages/inbox";
import { ProjectsPage } from "@/pages/projects";
import { SkillsPage } from "@/pages/skills";
import { MCPPage } from "@/pages/mcp";
import { HomePage } from "@/pages/home";
import { SettingsPage } from "@/pages/settings";
import { MaterialProvider } from "@/providers/material-provider";
import { ReviewProvider } from "@/components/steve/review-context";

export function App() {
    const navigate = useNavigate();
    return <FleetProvider><CoordinationProvider><IntentProvider onNavigate={() => navigate("/console")}><MaterialProvider><SideChatProvider><SelectionProvider><ReviewProvider><Shell /><DesktopOnboarding /></ReviewProvider></SelectionProvider></SideChatProvider></MaterialProvider></IntentProvider></CoordinationProvider></FleetProvider>;
}

function Shell() {
    const { snap, live } = useFleet();
    const { view: coordination, error: coordinationError } = useCoordination();
    const location = useLocation();
    const { locale, t } = useI18n();
    const desktop = useBreakpoint("xl");
    const tablet = useBreakpoint("sm");
    const [navCollapsed, setNavCollapsed] = useState(() => { try { return localStorage.getItem("steve.nav.collapsed") === "1"; } catch { return false; } });
    const [mobileNav, setMobileNav] = useState(false);
    const compact = navCollapsed || !desktop;
    const running = snap.tasks.filter((t) => t.execution === "running" && !t.parent).length;
    const up = snap.nodes.filter((n) => n.up).length;
    const broken = snap.sources.filter((s) => s.wired && s.error).length;
    const groups = [
        { id: "work", title: t("nav.work"), items: [
            { href: "/console", label: t("nav.console"), icon: Terminal, badge: 0 },
            { href: "/console?view=board", label: t("nav.tasks"), icon: ClipboardCheck, badge: running },
            { href: "/inbox", label: t("nav.inbox"), icon: Inbox01, badge: snap.inbox.length },
        ] },
        { id: "manage", title: t("nav.manage"), items: [
            { href: "/projects", label: t("nav.projects"), icon: Folder, badge: 0 },
            { href: "/fleet", label: t("nav.fleet"), icon: Server01, badge: 0 },
            { href: "/skills", label: t("nav.skills"), icon: PuzzlePiece01, badge: 0 },
            { href: "/mcp", label: t("nav.mcp"), icon: Dataflow03, badge: 0 },
            { href: "/home", label: t("nav.home"), icon: BookOpen01, badge: 0 },
        ] },
    ];
    const selected = location.pathname + location.search;
    const connection = t(live === "live" ? (broken ? "connection.partial" : "connection.live") : live === "unauthorized" ? "connection.unauthorized" : "connection.connecting");
    const devices = t("connection.devices", { online: number(up, locale), total: number(snap.nodes.length, locale) });
    const coordinatorName = coordination?.enabled ? coordination.nodes.find((node) => node.id === coordination.coordinator_id)?.name || coordination.coordinator_id || "" : coordination ? snap.hub.node : "";
    const coordinatorCurrent = live === "live" && !coordinationError && (!coordination?.enabled || coordination.authoritative);
    const coordinatedBy = coordinatorName ? t(coordinatorCurrent ? "connection.coordinatedBy" : "connection.lastCoordinator", { node: coordinatorName }) : t("connection.coordinatorUnknown");
    const navigation = (small: boolean) => <>
        <div className="app-brand">
            <span className="app-mark" aria-hidden="true"><Terminal /></span>
            {!small && <span><strong>Steve</strong><small>{t("app.workspace")}</small></span>}
        </div>
        <nav aria-label={t("nav.main")} className="app-navigation">
            {groups.map((group) => <div key={group.id} className="app-nav-group">
                {!small && <span className="app-nav-label">{group.title}</span>}
                {group.items.map((item) => <a key={item.href} href={"#" + item.href} aria-label={small ? item.label : undefined}
                    aria-current={(item.href === "/home" ? location.pathname === "/home" : item.href === "/console?view=board" ? location.pathname === "/console" && new URLSearchParams(location.search).get("view") === "board" : selected === item.href) ? "page" : undefined} title={small ? item.label : undefined}
                    className="app-nav-item" onClick={() => setMobileNav(false)}>
                    <item.icon aria-hidden="true" />
                    {!small && <span>{item.label}</span>}
                    {!!item.badge && <span className={small ? "app-nav-dot" : "app-nav-count"}>{small ? null : number(item.badge, locale)}</span>}
                </a>)}
            </div>)}
            <div className="app-nav-bottom">
                <a href="#/history" aria-label={small ? t("nav.history") : undefined} title={small ? t("nav.history") : undefined} aria-current={location.pathname === "/history" ? "page" : undefined} className="app-nav-item" onClick={() => setMobileNav(false)}><BookOpen01 aria-hidden="true" />{!small && <span>{t("nav.history")}</span>}</a>
            </div>
        </nav>
        <div className="app-sidebar-footer">
            <a href="#/fleet" className={`mb-2 flex min-h-10 min-w-0 rounded-md px-1.5 py-2 text-xs hover:bg-secondary_hover ${small ? "items-center justify-center" : "flex-col gap-1"}`} aria-label={coordinatedBy} title={`${coordinatedBy} · ${t("connection.coordinatorRole")}`} onClick={() => setMobileNav(false)}>
                {small ? <Server01 className="size-4 text-tertiary" aria-hidden="true" /> : <><span className="text-tertiary">{t(coordinatorCurrent ? "connection.coordinator" : "connection.lastCoordinatorRole")}</span><span className="truncate font-semibold text-primary">{coordinatorName || "—"}</span></>}
            </a>
            {small && <span role="status" className="app-compact-status" aria-label={connection} title={`${connection} · ${devices}`}><span className={`connection-dot ${live === "live" && !broken ? "connected" : ""}`} /></span>}
            {!small && <div className="app-connection" title={`${coordinatedBy} · ${devices}`}>
                <span className={`connection-dot ${live === "live" && !broken ? "connected" : ""}`} />
                <span>{connection}</span><span className="app-device-count">{t("connection.online", { count: number(up, locale) })}</span>
            </div>}
            <div className="app-sidebar-tools">
                <a href="#/settings?section=general" className="app-settings-link" aria-label={t("settingsPage.centerTitle")} title={small ? t("settingsPage.centerTitle") : undefined} aria-current={location.pathname === "/settings" ? "page" : undefined} onClick={() => setMobileNav(false)}><Settings01 aria-hidden="true" />{!small && <span>{t("settingsPage.centerTitle")}</span>}</a>
                {!small && <span className="app-version" title={snap.hub.node}>{snap.hub.version || "—"}</span>}
                {desktop && <button type="button" className="workbench-icon-button app-collapse" aria-label={small ? t("nav.expand") : t("nav.collapse")} title={small ? t("nav.expand") : t("nav.collapse")} onClick={() => { const next = !navCollapsed; setNavCollapsed(next); try { localStorage.setItem("steve.nav.collapsed", next ? "1" : "0"); } catch { /* Preference is optional. */ } }}><ChevronLeftDouble className={small ? "rotate-180" : ""} aria-hidden="true" /></button>}
            </div>
        </div>
    </>;
    return <div className="workbench-shell">
        <a className="skip-link" href="#main-content" onClick={(event) => { event.preventDefault(); document.getElementById("main-content")?.focus(); }}>{t("nav.skip")}</a>
        {tablet && <aside className={`app-sidebar ${compact ? "is-compact" : ""}`}>{navigation(compact)}</aside>}
        {!tablet && <div className="app-mobile-bar"><button type="button" className="workbench-icon-button" aria-label={t("nav.menu")} onClick={() => setMobileNav(true)}><Menu01 aria-hidden="true" /></button><strong>Steve</strong><a href="#/fleet" className="max-w-[60%] truncate rounded px-1 py-2 text-xs text-tertiary hover:text-primary" title={t("connection.coordinatorRole")}>{coordinatedBy}</a><span className={`connection-dot ${live === "live" ? "connected" : ""}`} title={connection} /></div>}
        {mobileNav && !tablet && <Sheet label={t("nav.menu")} side="left" width={260} onClose={() => setMobileNav(false)}><button type="button" className="sheet-close workbench-icon-button" aria-label={t("nav.close")} onClick={() => setMobileNav(false)}><X aria-hidden="true" /></button><div className="app-sidebar is-mobile">{navigation(false)}</div></Sheet>}
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
                <Route path="/settings" element={<SettingsPage />} />
                <Route path="/inbox" element={<InboxPage />} />
                <Route path="/ledger" element={<Navigate to="/inbox" replace />} />
                <Route path="/history" element={<HistoryPage />} />
                <Route path="/activity" element={<Navigate to="/history" replace />} />
            </Routes>
        </main>
    </div>;
}

export default function AppWithRouter() { return <HashRouter><App /></HashRouter>; }
