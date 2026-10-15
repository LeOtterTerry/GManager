import { defineConfig } from "vite";

const proxy = {
  "/api": "http://127.0.0.1:8787",
};

export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: 5173,
    proxy,
  },
  preview: {
    host: "127.0.0.1",
    port: 4173,
    proxy,
  },
});
