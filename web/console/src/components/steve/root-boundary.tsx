import { Component, type ReactNode } from "react";

// The last stop for a render error, including messages that could not be
// fetched for the first paint. It has no language to draw in, so it speaks
// both, and offers the one recovery a page can make on its own.
export class RootBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
    state = { failed: false };
    static getDerivedStateFromError() { return { failed: true }; }
    render() {
        if (!this.state.failed) return this.props.children;
        return <div role="alert" className="flex min-h-screen flex-col items-center justify-center gap-4 p-8 text-center text-sm text-secondary">
            <p lang="en">Steve could not load this page.</p>
            <p lang="zh-CN">Steve 无法加载此页面。</p>
            <button type="button" className="rounded-lg border border-secondary bg-primary px-3 py-2 font-semibold text-primary" onClick={() => window.location.reload()}>Reload · 重新加载</button>
        </div>;
    }
}
