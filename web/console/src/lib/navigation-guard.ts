let backGuard: ((event: PopStateEvent) => void) | undefined;

function onPopState(event: PopStateEvent) {
    backGuard?.(event);
}

// Install at bootstrap, before HashRouter can observe a navigation and unmount
// a dirty form. Lazy routes register their current guard, not another listener.
export function installBackNavigationGuard() {
    window.addEventListener("popstate", onPopState, true);
}

export function registerBackNavigationGuard(guard: (event: PopStateEvent) => void) {
    backGuard = guard;
    return () => {
        if (backGuard === guard) backGuard = undefined;
    };
}
