import type { MailLogin } from "@/lib/mail-types";

export function loginActive(job?: MailLogin) {
    return job?.state === "starting" || job?.state === "waiting";
}

export function loginLabel(job: MailLogin) {
    if (job.state === "authorized") return "mail.ready";
    if (job.state === "error") return "mail.loginFailed";
    if (job.stage === "queued") return "mail.loginQueuedStatus";
    if (job.stage === "onepassword_approval") return "mail.onePasswordApprovalStatus";
    if (job.needs_owner) return "mail.loginOwnerStatus";
    switch (job.stage) {
        case "username": case "account_choice": return "mail.loginUsernameStatus";
        case "password": case "password_option": return "mail.loginPasswordStatus";
        case "verification_code": case "otp_option": return "mail.loginOTPStatus";
        case "google_consent": return "mail.loginConsentStatus";
        case "opening_browser": return "mail.loginBrowserStatus";
        default: return "mail.signingIn";
    }
}
