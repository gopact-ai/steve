import { intlLocale, translate, type Locale } from "./i18n.ts";

export const number = (value: number, locale: Locale = "zh", options?: Intl.NumberFormatOptions) => new Intl.NumberFormat(intlLocale(locale), options).format(value);
export const dateTime = (value: string | Date, locale: Locale, options: Intl.DateTimeFormatOptions = {}) => {
    const date = value instanceof Date ? value : new Date(value);
    return Number.isNaN(date.getTime()) ? "—" : new Intl.DateTimeFormat(intlLocale(locale), options).format(date);
};
export const when = (t?: string, locale: Locale = "zh") => (t ? dateTime(t, locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "");
export const short = (s?: string, n = 12) => (s ? s.slice(0, n) : "");
export const relative = (t?: string, locale: Locale = "zh", now = Date.now()) => {
    if (!t) return "";
    const timestamp = new Date(t).getTime();
    if (Number.isNaN(timestamp)) return "—";
    const seconds = Math.max(0, Math.round((now - timestamp) / 1000));
    if (seconds < 5) return translate(locale, "format.now");
    const [amount, unit]: [number, Intl.RelativeTimeFormatUnit] = seconds < 60 ? [seconds, "second"] : seconds < 3600 ? [Math.round(seconds / 60), "minute"] : seconds < 86400 ? [Math.round(seconds / 3600), "hour"] : [Math.round(seconds / 86400), "day"];
    return new Intl.RelativeTimeFormat(intlLocale(locale), { numeric: "auto", style: "short" }).format(-amount, unit);
};
