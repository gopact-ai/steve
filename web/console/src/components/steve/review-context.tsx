import { createContext, lazy, Suspense, useCallback, useContext, useState, type ReactNode } from "react";
import type { ReviewRequest } from "./review-workspace";

const ReviewWorkspace = lazy(() => import("./review-workspace").then((module) => ({ default: module.ReviewWorkspace })));

const ReviewContext = createContext<(request: ReviewRequest) => void>(() => undefined);
const ReviewActiveContext = createContext(false);

export function ReviewProvider({ children }: { children: ReactNode }) {
    const [request, setRequest] = useState<ReviewRequest | null>(null);
    const open = useCallback((request: ReviewRequest) => setRequest(request), []);
    return <ReviewContext.Provider value={open}><ReviewActiveContext.Provider value={!!request}>{children}{request && <Suspense fallback={<p role="status" className="fixed bottom-4 right-4 z-[130] rounded-lg bg-primary p-4 text-sm text-secondary shadow-lg">打开代码工作区…</p>}><ReviewWorkspace key={request.attempt + ":" + (request.path || "")} request={request} onClose={() => setRequest(null)} /></Suspense>}</ReviewActiveContext.Provider></ReviewContext.Provider>;
}

export const useReview = () => useContext(ReviewContext);
export const useReviewActive = () => useContext(ReviewActiveContext);
