import type { HumanRequest, Selectors } from "../src/lib/types";

// These two wire contracts must not be declaration-merged into one Choice.
export const requestChoice: HumanRequest["choices"][number] = { label: "Approve", command: "/approve 1", danger: false };
export const selectorChoice: NonNullable<Selectors["models"]>[number] = { Value: "model-a", Label: "Model A", Detail: "Default" };
// @ts-expect-error A selector is not an executable human-request choice.
export const wrongRequest: HumanRequest["choices"][number] = selectorChoice;
// @ts-expect-error A command is not a model/option selector.
export const wrongSelector: NonNullable<Selectors["models"]>[number] = requestChoice;
