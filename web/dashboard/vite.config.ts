import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The CLI serves the dashboard same-origin from its REST port and embeds the
// build output via go:embed, so the build writes straight into the Go package's
// out/ directory. In `vite dev` the Connect API is proxied to a locally running
// `codefly server` (default REST port 10001) so the same relative request paths
// work in both dev and the embedded build.
const CLI_SERVER = process.env.CODEFLY_CLI_SERVER ?? "http://localhost:10001";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../../pkg/web/go-grpc/out",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/codefly.cli.v0.CLI": {
        target: CLI_SERVER,
        changeOrigin: true,
      },
    },
  },
});
