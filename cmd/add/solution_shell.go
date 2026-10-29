package add

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
)

// The page shell, written over the generic app the nextjs agent scaffolds.
//
// WHY THE CLI WRITES THIS AND NOT AN AGENT. The shell is a nextjs service, so
// the nextjs agent creates it — and that agent knows nothing about solutions:
// what it leaves is a standalone web app with placeholder login, signup and
// dashboard routes and a home page reading "Your app is running. Edit
// src/app/page.tsx to get started." The solution agent cannot write it either,
// because it runs for the BACKEND and the nextjs agent runs after it; anything
// it wrote here would be overwritten. The CLI is the only step that happens
// after both, and it is the only one that knows the shape has a page at all.
//
// WHAT THE SHELL IS FOR. A host federates the solution's ./Page remote and
// mounts it among its own surfaces. Where no host does, this shell is what
// serves that same page, so the two cannot drift: it imports the remote
// rather than reimplementing it.
//
// A SOLUTION HAS NO LOGIN OF ITS OWN, which is why the generic app's login
// and signup routes are deleted rather than filled in. The authority's edge
// is the browser's origin and signs the person in; the page holds no token
// and decides nothing. A solution that grew its own sign-in would be a second
// front door onto the same data.
func applySolutionShell(ctx context.Context, module *resources.Module, remotePackage string) error {
	app := filepath.Join(module.Dir(), "services", solutionPageService, "code", "src", "app")

	// The generic app's placeholders. They are not a starting point for a
	// solution: a solution never authenticates anyone.
	for _, placeholder := range []string{"login", "signup", "dashboard"} {
		if err := os.RemoveAll(filepath.Join(app, placeholder)); err != nil {
			return fmt.Errorf("cannot remove the generic app's %s route: %w", placeholder, err)
		}
	}

	for name, content := range map[string]string{
		"session.ts":       shellSession(module.Name),
		"solution-app.tsx": shellApp(module.Name, remotePackage),
		"page.tsx":         shellPage(),
	} {
		if err := shared.WriteFileAtomic(ctx, filepath.Join(app, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("cannot write the shell's %s: %w", name, err)
		}
	}
	if err := shellNextConfig(ctx, module, remotePackage); err != nil {
		return err
	}
	if err := shellDependsOnRemote(ctx, module, remotePackage); err != nil {
		return err
	}
	cli.Info("the shell serves the solution's ./Page (login routes removed: the edge signs the person in)")
	return nil
}

// shellPage is the route itself: a server component that renders the client
// half. The page is one person's session, read in the browser, so it is
// rendered per request and never prerendered — and `force-dynamic` is
// route-segment configuration, which only a server component may declare.
func shellPage() string {
	return `import SolutionApp from "./solution-app";

export const dynamic = "force-dynamic";

export default function Page() {
  return <SolutionApp />;
}
`
}

// shellSession reads the viewer from the authority's edge.
//
// The edge in front of this page is the host's door. The browser holds only
// its session cookie; every call goes through the edge, which sends it on
// with the person's Work Context. This file holds no token and decides
// nothing a call may do — it only reports who the edge says is here, and
// whether this solution is installed for them.
func shellSession(solution string) string {
	return fmt.Sprintf(`"use client";

import { useEffect, useState } from "react";

/** One target the person holds a Work Context at, and the installation it is sealed to. */
export type Installation = { id: string; target: string; namespace?: string };

/** The edge's /auth/session: who is here, and what they hold where. */
export type Session = {
  moduleApiBase?: string;
  personId: string;
  organizationId: string;
  personDisplayName?: string;
  organizationDisplayName?: string;
  roles: string[];
  installations: Installation[];
  /** Every target the authority refused an issue at, in its own words. */
  refused: Record<string, string>;
};

export type SessionState =
  | { status: "loading" }
  | { status: "signed-in"; session: Session }
  | { status: "error"; reason: string };

/** The sign-in page is the EDGE'S, never a route of this app. */
export function signInLocation(pathname: string, search: string): string {
  return "/auth/sign-in?return=" + encodeURIComponent(pathname + search);
}

/** Whether the person may use this solution: the edge's session names an installation of it. */
export function installed(session: Pick<Session, "installations">): boolean {
  return session.installations.some((installation) => installation.target === %q);
}

/** Why the edge's session names no installation, in the authority's own words. */
export function refusal(session: Pick<Session, "refused">): string | undefined {
  return session.refused[%q];
}

export function useSession(): SessionState {
  const [state, setState] = useState<SessionState>({ status: "loading" });
  useEffect(() => {
    let cancelled = false;
    fetch("/auth/session", { credentials: "include" })
      .then((response) => {
        // Not signed in: the edge's own sign-in page, with a return to here.
        if (response.status === 401 || response.status === 403) {
          window.location.assign(signInLocation(window.location.pathname, window.location.search));
          return null;
        }
        if (!response.ok) throw new Error("the edge answered " + response.status);
        return response.json();
      })
      .then((session: Session | null) => {
        if (!cancelled && session) setState({ status: "signed-in", session });
      })
      .catch((cause) => {
        if (!cancelled) setState({ status: "error", reason: String(cause) });
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return state;
}

export async function signOut(): Promise<void> {
  await fetch("/auth/sign-out", { method: "POST", credentials: "include" });
  window.location.assign("/");
}
`, solution, solution)
}

// shellApp mounts the remote's ./Page — the SAME module a host federates,
// imported rather than reimplemented, so the shell and the host show one page.
//
// It is loaded with ssr: false because that is how a host mounts it: in the
// browser. The server bundle never runs it, and compiling it there is what
// leaves the renderer resolving browser APIs it has none of.
func shellApp(solution, remotePackage string) string {
	return fmt.Sprintf(`"use client";

import dynamic from "next/dynamic";
import { installed, refusal, signOut, useSession } from "./session";

// The page renders in the browser only, as a host mounts it.
const SolutionPage = dynamic(() => import(%q), { ssr: false });

// Every call the page makes goes to this one same-origin base. The edge in
// front of this page routes /v1/%s to this solution's backend and sends each
// call on with the signed-in person's Work Context.
const apiBase = "/v1/%s";

// The page holds no token: the browser's cookie and the edge carry the viewer.
const noToken = () => null;

// Wrapped, never passed as `+"`fetch`"+` itself: called as a member of the
// binding, the browser's fetch refuses a `+"`this`"+` that is not the window.
const sameOrigin = (input: RequestInfo | URL, init?: RequestInit) => fetch(input, init);

export default function SolutionApp() {
  const signedIn = useSession();

  if (signedIn.status === "loading") return null;

  if (signedIn.status === "error") {
    return (
      <main style={{ padding: "2rem", maxWidth: "48rem" }}>
        <h1>This page is not behind its sign-in edge</h1>
        <p>
          {signedIn.reason}. Open this page at the address the authority's edge
          serves, never at this shell's own port — the edge is what signs the
          person in and routes <code>{apiBase}</code> to the backend.
        </p>
      </main>
    );
  }

  const { session } = signedIn;
  if (!installed(session)) {
    return (
      <main style={{ padding: "2rem", maxWidth: "48rem" }}>
        <h1>%s is not available to you here</h1>
        <p>
          {refusal(session) ??
            "The edge's session names neither an installation of this solution nor the authority's reason for refusing one: the edge does not route it."}
        </p>
      </main>
    );
  }

  return (
    <main style={{ padding: "1.5rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
      <header style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap" }}>
        <h1 style={{ fontSize: "1.25rem", fontWeight: 600, margin: 0 }}>%s</h1>
        <span style={{ fontSize: "0.875rem", opacity: 0.7 }}>
          {session.personDisplayName?.trim() || session.personId} ·{" "}
          {session.organizationDisplayName?.trim() || session.organizationId}
          {session.roles.length ? " · " + session.roles.join(", ") : ""}
        </span>
        <button type="button" onClick={() => void signOut()}>Sign out</button>
      </header>
      <SolutionPage
        solutionId=%q
        apiBase={apiBase}
        getAccessToken={noToken}
        authedFetch={sameOrigin}
        organizationId={session.organizationId}
        viewerId={session.personId}
        moduleApiBase={session.moduleApiBase}
      />
    </main>
  );
}
`, remotePackage+"/Page", solution, solution, titleFromName(solution), titleFromName(solution), solution)
}

// shellNextConfig lets the shell compile the remote's source.
//
// It is NOT wiki's config. wiki aliases React and drops the page from the
// server bundle through a `webpack` block, and that block is a hard ERROR on
// the Next this agent scaffolds: Next 16 runs Turbopack by default and
// refuses a webpack config it was not told to use. Neither alias is needed
// here — npm hoists one React for the linked remote, and ssr: false is what
// keeps the page out of the server bundle.
func shellNextConfig(ctx context.Context, module *resources.Module, remotePackage string) error {
	path := filepath.Join(module.Dir(), "services", solutionPageService, "code", "next.config.ts")
	return shared.WriteFileAtomic(ctx, path, []byte(shellNextConfigSource(remotePackage)), 0o644)
}

// shellNextConfigSource is the config itself, separate from writing it so the
// two rules that cost a run each — the Turbopack root and the absence of a
// webpack block — are assertable without scaffolding a module.
func shellNextConfigSource(remotePackage string) string {
	return fmt.Sprintf(`import type { NextConfig } from "next";
import { resolve } from "node:path";

const config: NextConfig = {
  // The remote lives beside this app, not under it.
  experimental: { externalDir: true },
  // It ships its SOURCE, as the host's remote build reads it, so the shell
  // compiles it rather than consuming a build.
  transpilePackages: [%q],
  turbopack: {
    // THE BUILD ROOT IS THE SOLUTION'S, not this app's. The remote is linked
    // into node_modules as a symlink to ../../fe-remote, and Turbopack
    // refuses to follow a symlink out of its root: "Symlink
    // node_modules/%s/src/Page.tsx could not be resolved: the symlink target
    // leaves the configured filesystem roots", served as a 500 while the
    // link, the package's exports map and the file are all correct.
    root: resolve("../../.."),
    resolveExtensions: [".tsx", ".ts", ".jsx", ".js", ".mjs", ".json"],
  },
};

export default config;
`, remotePackage, remotePackage)
}

// shellDependsOnRemote links the remote into the shell's npm project, which is
// how `import "<name>/Page"` resolves at all. It is a relative file:
// dependency, so the link holds wherever the repository is checked out.
func shellDependsOnRemote(ctx context.Context, module *resources.Module, remotePackage string) error {
	path := filepath.Join(module.Dir(), "services", solutionPageService, "code", "package.json")
	existing, err := os.ReadFile(path) //nolint:gosec // a path inside the scaffolded module
	if err != nil {
		return fmt.Errorf("cannot read the shell's package.json: %w", err)
	}
	updated, err := addJSONDependency(existing, remotePackage, "file:../../fe-remote")
	if err != nil {
		return fmt.Errorf("cannot link the remote into the shell: %w", err)
	}
	if err = shared.WriteFileAtomic(ctx, path, updated, 0o644); err != nil {
		return err
	}
	// The lock has to follow the manifest. The nextjs agent ships one, and
	// its runner installs with `npm ci`, which refuses a lock that does not
	// match package.json — so adding the remote without relocking fails the
	// shell at runtime-run ("npm ci failed: exit status 1"), long after the
	// scaffold reported success.
	//
	// Resolution needs the network, so a failure here is reported and not
	// fatal: the solution is complete and the author runs the one command.
	command := exec.CommandContext(ctx, "npm", "install", "--package-lock-only", "--silent")
	command.Dir = filepath.Dir(path)
	if output, lockErr := command.CombinedOutput(); lockErr != nil {
		cli.Warning("could not relock the shell after linking the remote (%v). Its runner installs with `npm ci`, which needs the lock to match package.json: run `npm install --package-lock-only` in services/%s/code.\n%s",
			lockErr, solutionPageService, strings.TrimSpace(string(output)))
	}
	return nil
}

// addJSONDependency adds one dependency to a package.json without reformatting
// the rest of it. encoding/json would reorder every key and drop the file's
// own formatting, which makes the agent's template unreadable in a diff; this
// edits the dependencies object in place.
func addJSONDependency(manifest []byte, name, version string) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(manifest, &document); err != nil {
		return nil, err
	}
	dependencies, _ := document["dependencies"].(map[string]any)
	if dependencies == nil {
		dependencies = map[string]any{}
	}
	if existing, present := dependencies[name]; present && existing == version {
		return manifest, nil
	}
	dependencies[name] = version
	document["dependencies"] = dependencies
	rendered, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(rendered, '\n'), nil
}
