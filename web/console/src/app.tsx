import { useCallback, useMemo } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { Activity, BookOpen01, ClipboardCheck, GitBranch01, Moon01, Server01, Sun, Terminal } from "@untitledui/icons";
import { NavItemBase } from "@/components/application/app-navigation/base-components/nav-item";
import { Badge } from "@/components/base/badges/badges";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
import { token, when } from "@/lib/api";
import { useTheme } from "@/providers/theme-provider";
import { ActivityPage } from "@/pages/activity";
import { ConsolePage } from "@/pages/console";
import { FleetPage } from "@/pages/fleet";
import { LedgerPage } from "@/pages/ledger";
import { PlansPage } from "@/pages/plans";
import { TasksPage } from "@/pages/tasks";

export function App() {
    const navigate = useNavigate();
    const toConsole = useCallback(() => navigate("/console"), [navigate]);
    return (
        <FleetProvider>
            <IntentProvider onNavigate={toConsole}>
                <Shell />
            </IntentProvider>
        </FleetProvider>
    );
}

function Shell() {
    const { snap, live } = useFleet();
    const { pathname } = useLocation();
    const navigate = useNavigate();
    const { theme, setTheme } = useTheme();

    const attention = snap.facts.disclosures.length + snap.facts.effects.length;
    const running = snap.tasks.filter((t) => ["running", "blocked", "review"].includes(t.state)).length;
    const items = useMemo(() => [
        { href: "/console", label: "Console", icon: Terminal },
        { href: "/fleet", label: "Fleet", icon: Server01, badge: snap.nodes.length ? `${snap.nodes.filter((n) => n.up).length}/${snap.nodes.length}` : undefined },
        { href: "/tasks", label: "Tasks", icon: ClipboardCheck, badge: running || undefined },
        { href: "/plans", label: "Plans", icon: GitBranch01, badge: snap.plans.length || undefined },
        { href: "/ledger", label: "Ledger", icon: BookOpen01, badge: attention || undefined, hot: attention > 0 },
        { href: "/activity", label: "Activity", icon: Activity },
    ], [snap, attention, running]);

    const dark = theme === "dark" || (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);

    return (
        <div className="flex h-dvh bg-secondary">
            <aside className="flex w-64 shrink-0 flex-col border-r border-secondary bg-primary">
                <div className="flex items-center gap-2.5 px-5 pt-5 pb-3">
                    <span className="flex size-8 items-center justify-center rounded-lg bg-brand-solid text-white shadow-xs">
                        <Terminal className="size-4" />
                    </span>
                    <div className="leading-tight">
                        <div className="text-md font-semibold text-primary">steve</div>
                        <div className="text-xs text-tertiary">{snap.hub.node ? `hub · ${snap.hub.node}` : "console"}</div>
                    </div>
                </div>
                <ul className="flex flex-col gap-0.5 px-4 pt-2">
                    {items.map((item) => (
                        <li key={item.href}>
                            <NavItemBase
                                type="link"
                                href={"#" + item.href}
                                current={pathname.startsWith(item.href)}
                                icon={item.icon}
                                badge={item.badge ? (
                                    <Badge type="pill-color" size="sm" color={item.hot ? "warning" : "gray"}>{item.badge}</Badge>
                                ) : undefined}
                                onClick={(e) => { e.preventDefault(); navigate(item.href); }}
                            >
                                {item.label}
                            </NavItemBase>
                        </li>
                    ))}
                </ul>
                <div className="mt-auto flex flex-col gap-2 border-t border-secondary px-5 py-4 text-xs text-tertiary">
                    <div className="flex items-center gap-2">
                        <span className={`size-2 rounded-full ${live === "live" ? "bg-success-solid" : live === "unauthorized" ? "bg-error-solid" : "bg-warning-solid"}`} />
                        <span className="capitalize">{live}</span>
                        <span className="ml-auto">{snap.at ? when(snap.at) : ""}</span>
                    </div>
                    <div>{snap.attempts.length} running · {snap.tasks.length} tasks · {snap.plans.length} plans</div>
                    <div className="flex items-center justify-between">
                        <span>{token ? "token ok" : "no token"}</span>
                        <ButtonUtility size="xs" color="tertiary" tooltip={dark ? "Light" : "Dark"} icon={dark ? Sun : Moon01} onClick={() => setTheme(dark ? "light" : "dark")} />
                    </div>
                </div>
            </aside>
            <main className="flex min-w-0 flex-1 flex-col">
                {live === "unauthorized" && (
                    <div className="border-b border-error bg-error-primary px-6 py-2 text-sm text-error-primary">
                        Token rejected — open the page with <code>?token=…</code>.
                    </div>
                )}
                <div className="min-h-0 flex-1 overflow-auto">
                    <Routes>
                        <Route path="/" element={<Navigate to="/console" replace />} />
                        <Route path="/console" element={<ConsolePage />} />
                        <Route path="/fleet" element={<FleetPage />} />
                        <Route path="/tasks" element={<TasksPage />} />
                        <Route path="/plans" element={<PlansPage />} />
                        <Route path="/ledger" element={<LedgerPage />} />
                        <Route path="/activity" element={<ActivityPage />} />
                        <Route path="*" element={<Navigate to="/console" replace />} />
                    </Routes>
                </div>
            </main>
        </div>
    );
}
