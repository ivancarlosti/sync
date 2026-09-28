// Vite configuration for the Sync SPA.
//
// The build lands in `web/dist`, which is what `web/embed.go` embeds with
// `//go:embed all:dist`, so the Go runtime serves the SPA without Node.
//
// In development `npm run dev` proxies `/api` to the Go server, which keeps the
// session cookie on a single origin (no CORS, no SameSite surprises).
import { fileURLToPath, URL } from 'node:url';

import vue from '@vitejs/plugin-vue';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'VITE_');
  const apiTarget = env.VITE_DEV_API ?? 'http://127.0.0.1:3000';

  return {
    plugins: [vue()],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    build: {
      outDir: 'dist',
      // A stale asset from a previous build would be embedded into the binary,
      // so the directory is emptied on every build.
      emptyOutDir: true,
      // go:embed cannot represent symlinks or external chunks, so everything the
      // entry document needs must be a plain file inside dist.
      assetsDir: 'assets',
      sourcemap: false,
      chunkSizeWarningLimit: 900,
    },
    server: {
      port: 5173,
      proxy: {
        '/api': { target: apiTarget, changeOrigin: false },
      },
    },
    preview: {
      port: 4173,
    },
  };
});
