import type { UsageResponse } from "../types";
import { request } from "../http";

export async function fetchUsage(signal?: AbortSignal): Promise<UsageResponse> {
    const response = await request<UsageResponse>("/usage", { signal });
    const source = response.sources?.find((item) => item.name === "ledger-usage");
    if (!response.at || !source || (source.wired && !source.error && !response.usage)) {
        throw new Error("Invalid usage response");
    }
    if (response.usage && ["1d", "7d", "30d"].some((range) => !response.usage?.periods?.[range as "1d" | "7d" | "30d"])) {
        throw new Error("Invalid usage periods");
    }
    return response;
}
