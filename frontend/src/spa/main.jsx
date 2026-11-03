// Entrada da SPA em Vite.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@/app/globals.css";
import "./fonts.css";
import App from "./App.jsx";

createRoot(document.getElementById("root")).render(
  <StrictMode>
    <App />
  </StrictMode>
);
