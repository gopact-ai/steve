const backGuards = new Map<(event: PopStateEvent) => void, number>();

function onPopState(event: PopStateEvent) {
    // Several dirty forms may coexist. A rejected navigation must not reach
    // later guards or the router; released children cannot replace a parent.
    for (const [guard] of [...backGuards].sort((a, b) => b[1] - a[1])) {
        guard(event);
        if (event.cancelBubble) break;
    }
}

// Install at bootstrap, before HashRouter can observe a navigation and unmount
// a dirty form. Lazy routes register guards, not another window listener.
export function installBackNavigationGuard() {
    window.addEventListener("popstate", onPopState, true);
}

export function registerBackNavigationGuard(guard: (event: PopStateEvent) => void, priority = 0) {
    backGuards.set(guard, priority);
    return () => { backGuards.delete(guard); };
}
