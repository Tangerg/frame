import { defineConfig } from "vite";

export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: 5174,
    proxy: {
      "/api": "http://127.0.0.1:8088",
      "/frame": "http://127.0.0.1:8088",
    },
  },
});
