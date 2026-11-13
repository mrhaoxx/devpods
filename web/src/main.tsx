import React from "react";
import ReactDOM from "react-dom/client";
import "./index.css";
import { initTheme } from "./app/theme";
import { App } from "./app/App";
import { installSideButtons } from "./lib/sideButtons";

initTheme();
installSideButtons();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
