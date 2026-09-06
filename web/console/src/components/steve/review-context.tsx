import { createContext, useCallback, useContext, useState, type ReactNode } from "react";
import { ReviewWorkspace, type ReviewRequest } from "./review-workspace";

const ReviewContext = createContext<(request: ReviewRequest) => void>(() => undefined);
const ReviewActiveContext = createContext(false);

export function ReviewProvider({ children }: { children: ReactNode }) {
    const [request, setRequest] = useState<ReviewRequest | null>(null);
    const open = useCallback((request: ReviewRequest) => setRequest(request), []);
    return <ReviewContext.Provider value={open}><ReviewActiveContext.Provider value={!!request}>{children}{request && <ReviewWorkspace key={request.attempt + ":" + (request.path || "")} request={request} onClose={() => setRequest(null)} />}</ReviewActiveContext.Provider></ReviewContext.Provider>;
}

export const useReview = () => useContext(ReviewContext);
export const useReviewActive = () => useContext(ReviewActiveContext);
