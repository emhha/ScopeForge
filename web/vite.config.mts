import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig, loadEnv } from "vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const backend = env.AUTOPENTEST_API || "http://localhost:8787";
  return {
    plugins: [react({ babel: { plugins: ["babel-plugin-react-compiler"] } })],
    resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
    build: { outDir: "out" },
    server: {
      host: "0.0.0.0",
      port: 5173,
      strictPort: true,
      proxy: { "/api": { target: backend, changeOrigin: true } },
    },
    preview: {
      port: 5173,
      strictPort: true,
      proxy: { "/api": { target: backend, changeOrigin: true } },
    },
  };
});
