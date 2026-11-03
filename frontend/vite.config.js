import { defineConfig, transformWithOxc } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

const backendTarget = process.env.SPA_API_TARGET || "http://localhost:20128";
const srcRoot = fileURLToPath(new URL("./src", import.meta.url));

// O App Router permite JSX em arquivos .js; o pipeline Vite/oxc decide a
// linguagem pela extensão. Este plugin pré-processa todo .js de src com o
// transform oficial do Vite forçando o parser JSX (runtime automatic),
// devolvendo código e sourcemap para o restante da cadeia.
function jsxInJs() {
  return {
    name: "vansgo:jsx-in-js",
    enforce: "pre",
    async transform(code, id) {
      const file = id.split("?")[0];
      if (!file.endsWith(".js") || !file.startsWith(srcRoot + "/")) return;
      const result = await transformWithOxc(code, file + ".jsx", {
        jsx: { runtime: "automatic" },
      });
      return { code: result.code, map: result.map ?? undefined };
    },
  };
}

// Fundação Vite da SPA.
// JSX em .js é pré-processado pelo plugin jsxInJs; imports @/compat/* fornecem
// as APIs de navegação/imagem/link/dynamic sobre react-router e React.lazy.
export default defineConfig({
  plugins: [jsxInJs(), react()],
  resolve: {
    alias: {
      "@": srcRoot,
      // open-sse/providers/shared.js importa "os" para fingerprints server-side;
      // shim neutro evita módulo Node no bundle (nenhum uso client).
      os: fileURLToPath(new URL("./src/compat/node_os.js", import.meta.url)),
      "node:os": fileURLToPath(new URL("./src/compat/node_os.js", import.meta.url)),
    },
  },
  server: {
    proxy: {
      // Nota: respostas SSE ficam presas no proxy do dev server do Vite
      // (buffering do middleware; flush em proxyRes não resolveu). O smoke
      // valida SSE direto na origem Go; em produção o Go serve a SPA na
      // mesma origem, sem proxy.
      "/api": { target: backendTarget, changeOrigin: true, ws: true },
    },
  },
  build: {
    outDir: "dist",
  },
  // Módulos compartilhados open-sse leem process.env em nível de módulo
  // (ex.: config/grokCli.js). No navegador essas leituras devem ser vazias.
  define: {
    "process.env": {},
  },
});
