import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// roster passes --port at run time so we don't bake one in here.
// strictPort makes Vite fail loudly if the assigned port is taken
// instead of silently picking another (which would break the
// fleetview iframe pointing at a stale port).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    strictPort: true,
    host: "127.0.0.1",
  },
});
