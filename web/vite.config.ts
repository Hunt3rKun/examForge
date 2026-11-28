import { readFileSync, readdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

const target = process.env.VITE_API_PROXY_TARGET ?? "http://127.0.0.1:8080";

function pdfSupportFiles(): Plugin {
  const pdfjs = dirname(createRequire(import.meta.url).resolve("pdfjs-dist/package.json"));
  const files = new Map<string, string>();
  for (const dir of ["cmaps", "standard_fonts"]) {
    for (const entry of readdirSync(join(pdfjs, dir), { withFileTypes: true })) {
      if (entry.isFile()) files.set(`pdfjs/${dir}/${entry.name}`, join(pdfjs, dir, entry.name));
    }
  }
  return {
    name: "examforge-pdf-support-files",
    configureServer(server) {
      server.middlewares.use((request, response, next) => {
        if (request.method !== "GET" && request.method !== "HEAD") return next();
        const pathname = request.url?.split("?", 1)[0];
        if (!pathname?.startsWith(server.config.base)) return next();
        // Look up exact package filenames instead of resolving request paths on disk.
        const file = files.get(pathname.slice(server.config.base.length));
        if (!file) return next();
        void readFile(file).then(bytes => {
          response.setHeader("Content-Type", "application/octet-stream");
          response.setHeader("Content-Length", bytes.length);
          response.end(request.method === "HEAD" ? undefined : bytes);
        }, next);
      });
    },
    generateBundle() {
      for (const [fileName, file] of files) this.emitFile({ type: "asset", fileName, source: readFileSync(file) });
    },
  };
}

export default defineConfig({
  plugins: [react(), pdfSupportFiles()],
  server: {
    proxy: {
      "/api": { target, changeOrigin: false },
      "/healthz": { target, changeOrigin: false },
      "/readyz": { target, changeOrigin: false },
    },
  },
});
