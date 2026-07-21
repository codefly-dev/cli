import type { ReactNode } from "react";

export function Loading() {
  return <div className="state muted">Loading…</div>;
}

export function ErrorBox({ error }: { error: Error }) {
  return (
    <div className="state error">
      <strong>Could not reach the CLI.</strong>
      <div className="muted">{error.message}</div>
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="state muted">{children}</div>;
}
