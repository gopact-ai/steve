import { useI18n } from "@/providers/locale-provider";
import { Component, createContext, lazy, Suspense, useCallback, useContext, useState, type ReactNode } from "react";
import type { ReviewRequest } from "./review-workspace";

const ReviewWorkspace = lazy(() => import("./review-workspace").then((module) => ({ default: module.ReviewWorkspace })));

const ReviewContext = createContext<(request: ReviewRequest) => void>(() => undefined);
const ReviewActiveContext = createContext(false);

export function ReviewProvider({ children }: { children: ReactNode }) {
    const { t } = useI18n();
    const [request, setRequest] = useState<ReviewRequest | null>(null);
    const open = useCallback((request: ReviewRequest) => setRequest(request), []);
    return <ReviewContext.Provider value={open}><ReviewActiveContext.Provider value={!!request}>{children}{request && <ReviewLoadBoundary key={request.attempt+":"+(request.path||"")} onClose={()=>setRequest(null)} message={t("console.reviewLoadFailed")} closeLabel={t("console.back")}><Suspense fallback={<p role="status" className="fixed bottom-4 right-4 z-[130] rounded-lg bg-primary p-4 text-sm text-secondary shadow-lg">{t("console.openingReview")}</p>}><ReviewWorkspace key={request.attempt + ":" + (request.path || "")} request={request} onClose={() => setRequest(null)} /></Suspense></ReviewLoadBoundary>}</ReviewActiveContext.Provider></ReviewContext.Provider>;
}

export const useReview = () => useContext(ReviewContext);
export const useReviewActive = () => useContext(ReviewActiveContext);

class ReviewLoadBoundary extends Component<{children:ReactNode;onClose:()=>void;message:string;closeLabel:string},{failed:boolean}> {
    state={failed:false};
    static getDerivedStateFromError(){return{failed:true}}
    render(){return this.state.failed?<div role="alert" className="fixed bottom-4 left-1/2 z-[150] max-w-md -translate-x-1/2 rounded-lg border border-secondary bg-primary p-4 text-sm text-primary shadow-lg"><p>{this.props.message}</p><button type="button" className="mt-3 underline" onClick={this.props.onClose}>{this.props.closeLabel}</button></div>:this.props.children}
}
