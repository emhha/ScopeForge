import { createRoot } from "react-dom/client";
import { App } from "./application";
import { applyInitialPreferences } from "./scripts/theme-boot";
import "./lib/fonts/fonts.css";
import "./app/globals.css";

applyInitialPreferences();
createRoot(document.getElementById("root")!).render(<App />);
