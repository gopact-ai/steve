import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { HashRouter } from "react-router";
import { App } from "@/app";
import { RouteProvider } from "@/providers/router-provider";
import { ThemeProvider } from "@/providers/theme-provider";
import { LocaleProvider } from "@/providers/locale-provider";
import "@/styles/globals.css";

// Hash routes keep the token in the query string across every page and
// reload; the hub serves one shell for all of them.
createRoot(document.getElementById("root")!).render(
    <StrictMode>
        <LocaleProvider><ThemeProvider defaultTheme="system">
            <HashRouter>
                <RouteProvider>
                    <App />
                </RouteProvider>
            </HashRouter>
        </ThemeProvider></LocaleProvider>
    </StrictMode>,
);
