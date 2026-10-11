import { Badge } from "@/components/base/badges/badges";
import { useI18n } from "@/providers/locale-provider";
import type { EventDelivery } from "@/lib/assistant-types";

const labels = {
    pending: "assistant.deliveryPending", claimed: "assistant.deliveryClaimed", accepted: "assistant.deliveryAccepted",
    done: "assistant.deliveryDone", failed: "assistant.failed", unknown: "assistant.unknown", cancelled: "assistant.deliveryCancelled",
} as const;

export function DeliveryState({ state, mode, className }: { state: EventDelivery["state"]; mode: EventDelivery["rule"]["mode"]; className?: string }) {
    const { t } = useI18n();
    if (mode === "inbox" && state === "done") return <Badge className={className} type="pill-color" color="gray" size="sm">{t("assistant.deliveryToInbox")}</Badge>;
    const color = state === "done" ? "success" : state === "failed" ? "error" : state === "unknown" ? "warning" : ["claimed", "accepted"].includes(state) ? "brand" : "gray";
    return <Badge className={className} type="pill-color" color={color} size="sm">{labels[state] ? t(labels[state]) : state}</Badge>;
}
