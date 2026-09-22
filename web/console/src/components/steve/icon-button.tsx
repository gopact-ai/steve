import { cloneElement, createElement, isValidElement } from "react";
import type { ButtonProps as AriaButtonProps } from "react-aria-components";
import { ButtonUtility, type ButtonProps } from "@/components/base/buttons/button-utility";
import { isReactComponent } from "@/utils/is-react-component";

export interface IconButtonProps extends Omit<ButtonProps, "aria-label" | "children"> {
    label: string;
}

type IconAttributes = { "data-icon"?: string | boolean; "aria-hidden"?: boolean | "true" | "false" };

export function IconButton({ label, icon, size = "sm", color = "tertiary", disabled, isDisabled, title, tooltip, tooltipPlacement, slot, ...props }: IconButtonProps) {
    const element = isReactComponent(icon) ? createElement(icon) : icon;
    let normalizedIcon = element;
    if (isValidElement<IconAttributes>(element)) {
        normalizedIcon = cloneElement(element, {
            "data-icon": element.props["data-icon"] ?? true,
            "aria-hidden": element.props["aria-hidden"] ?? true,
        });
    }
    // Retain native attributes filtered by React Aria (e.g. title and
    // aria-keyshortcuts), while its generated handlers and ref remain authoritative.
    const nativeProps: Pick<AriaButtonProps, "render"> = {
        render: (domProps) => <button {...props} {...domProps} title={title} />,
    };

    return <ButtonUtility {...props} {...nativeProps} slot={slot} tooltip={tooltip} tooltipPlacement={tooltipPlacement} aria-label={label} icon={normalizedIcon} size={size} color={color} isDisabled={isDisabled || disabled} />;
}
