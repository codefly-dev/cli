package add

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// The module page's remote: services/fe-remote, an npm package exposing
// ./Page through Module Federation. A host mounts it with its page binding,
// and the page calls the module on the host's module edge (moduleApiBase,
// the `/_browser` door) — never on apiBase, which is the host's relay for a
// solution runtime's /v1/<alias> routes and which a module's Connect face does
// not publish. Its calls go through the module's generated client: the page
// states no procedure path of its own.
//
// It depends on nothing a registry token guards. The page is the module's;
// a kit it wants is the author's to add.

// modulePageRemote is every file of the remote, by path relative to it.
// clientDir is the generated client's TypeScript package, relative to the
// remote; client is its package name.
func modulePageRemote(module, title, client, clientDir string) map[string]string {
	method := pageOperations[1] // the read the page opens with
	return map[string]string{
		"package.json":      remotePackageJSON(module, client, clientDir),
		"rsbuild.config.ts": remoteRsbuildConfig(module),
		"tsconfig.json":     remoteTSConfig,
		".gitignore":        "node_modules\ndist\n",
		"src/Page.tsx":      remotePage(title, client, pageProtoService(module), lowerFirst(method.method), method.method),
	}
}

func remotePackageJSON(module, client, clientDir string) string {
	return fmt.Sprintf(`{
  "name": %[1]q,
  "private": true,
  "version": "0.0.0",
  "description": "The %[2]s module's page: a Module Federation remote exposing ./Page, which calls the module on the host's module edge through its generated client.",
  "type": "module",
  "exports": {
    "./Page": "./src/Page.tsx"
  },
  "scripts": {
    "client": "cd %[4]s && npm install --no-audit --no-fund && npm run build",
    "build": "rsbuild build",
    "typecheck": "tsc --noEmit"
  },
  "dependencies": {
    "@bufbuild/protobuf": "^2.17.0",
    "@connectrpc/connect": "^2.3.0",
    "@connectrpc/connect-web": "^2.3.0",
    %[3]q: "file:%[4]s",
    "react": "^19.2.0",
    "react-dom": "^19.2.0"
  },
  "devDependencies": {
    "@module-federation/rsbuild-plugin": "2.9.0",
    "@rsbuild/core": "^2.2.8",
    "@rsbuild/plugin-react": "^2.1.0",
    "@types/react": "^19.2.0",
    "typescript": "^5.9.3"
  }
}
`, module+"-fe-remote", module, client, filepath.ToSlash(clientDir))
}

// remoteRsbuildConfig shares React with the host as ONE instance: a second
// copy across the federation boundary is a second React, and the page fails
// to mount inside the host.
func remoteRsbuildConfig(module string) string {
	return fmt.Sprintf(`import { pluginModuleFederation } from "@module-federation/rsbuild-plugin";
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";

// The host's own React, never a bundled copy.
const singleton = { singleton: true, requiredVersion: false } as const;

export default defineConfig({
  plugins: [
    pluginReact(),
    pluginModuleFederation({
      // Emitted as a JavaScript global, so an identifier.
      name: %q,
      exposes: { "./Page": "./src/Page.tsx" },
      dts: false,
      shared: { "react": singleton, "react/jsx-runtime": singleton, "react-dom": singleton },
    }),
  ],
  server: { port: Number(process.env.PORT ?? 3100), cors: true },
  // "auto": chunk URLs resolve from wherever the remote entry was loaded, so
  // one build is servable from any origin. Styles are inlined into the JS: a
  // host's CSP refuses a cross-origin stylesheet.
  output: { assetPrefix: process.env.ASSET_PREFIX ?? "auto", injectStyles: true },
});
`, remoteGlobalName(module))
}

const remoteTSConfig = `{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["DOM", "DOM.Iterable", "ES2022"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noEmit": true,
    "skipLibCheck": true,
    "types": ["@rsbuild/core/types"]
  },
  "include": ["src", "rsbuild.config.ts"]
}
`

func remotePage(title, client, protoService, call, method string) string {
	return fmt.Sprintf(`import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
// The module's own service, from its generated client (codefly generate client).
import { %[3]s } from %[2]q;

/**
 * The fields of the host's page binding this page reads, by structure: a host
 * written against that binding mounts it unchanged. The title and order the
 * host's shell shows are not here; they are declared once, beside the entry
 * service, in solution.frontend.codefly.yaml.
 */
export interface PageProps {
  /** The host's relay for a solution runtime. A module page calls nothing on it. */
  apiBase?: string;
  /**
   * The host's module edge. The module's own procedures are called here,
   * <moduleApiBase>/<package.Service>/<Method>: the edge checks the person's
   * Work Context against the method's policy and forwards the call through
   * the authority's hop. Absent, the module is not reachable and nothing is
   * inferred in its place.
   */
  moduleApiBase?: string;
  /** The viewer's bearer, read on every call. */
  getAccessToken?: () => string | null;
  /** The host's bound fetch (its refresh-then-retry), used when offered. */
  authedFetch?: typeof fetch;
}

type Read = { status: "loading" } | { status: "read"; entries: string[] } | { status: "refused"; reason: string };

export default function Page({ moduleApiBase, getAccessToken, authedFetch }: PageProps) {
  const edge = moduleApiBase?.trim().replace(/\/+$/, "");
  // Memoized: a new client is a new reader to the browser.
  const client = useMemo(() => {
    if (!edge) return undefined;
    const send = authedFetch ?? fetch;
    const transport = createConnectTransport({
      baseUrl: edge,
      fetch: (input, init) => {
        const headers = new Headers(init?.headers);
        const token = getAccessToken?.();
        if (token) headers.set("authorization", `+"`Bearer ${token}`"+`);
        return send(input, { ...init, headers });
      },
    });
    return createClient(%[3]s, transport);
  }, [edge, getAccessToken, authedFetch]);

  const [read, setRead] = useState<Read>({ status: "loading" });
  useEffect(() => {
    if (!client) return;
    let current = true;
    // %[5]s needs the role's read scope; a person holding no role is refused
    // here, by the edge, in its own words.
    client.%[4]s({}).then(
      (answer) => current && setRead({ status: "read", entries: answer.entries }),
      (cause: unknown) => current && setRead({ status: "refused", reason: String(cause) }),
    );
    return () => {
      current = false;
    };
  }, [client]);

  if (!edge) return <p>The host names no module edge (moduleApiBase), so %[1]s is not reachable here.</p>;
  return (
    <section>
      <h1>%[1]s</h1>
      {read.status === "loading" && <p>Loading…</p>}
      {read.status === "refused" && <p role="alert">{read.reason}</p>}
      {read.status === "read" && (read.entries.length === 0 ? <p>No entries yet.</p> : <ul>{read.entries.map((entry) => <li key={entry}>{entry}</li>)}</ul>)}
    </section>
  );
}
`, title, client, protoService, call, method)
}

// remoteGlobalName is the Module Federation container name: a JavaScript
// identifier derived from the module name.
func remoteGlobalName(module string) string {
	var out strings.Builder
	upper := false
	for _, r := range module {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper && out.Len() > 0 {
			r = unicode.ToUpper(r)
		}
		upper = false
		out.WriteRune(r)
	}
	return out.String() + "Page"
}

func lowerFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToLower(value[:1]) + value[1:]
}
