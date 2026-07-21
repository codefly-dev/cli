import { useState } from "react";
import { cli } from "./api/cli";
import { useAsync } from "./hooks/useAsync";
import { ServicesView } from "./views/ServicesView";
import { LogsView } from "./views/LogsView";
import { GraphView } from "./views/GraphView";
import { ConfigView } from "./views/ConfigView";

type Tab = "services" | "logs" | "graph" | "config";

const TABS: { id: Tab; label: string }[] = [
  { id: "services", label: "Services" },
  { id: "logs", label: "Logs" },
  { id: "graph", label: "Dependency Graph" },
  { id: "config", label: "Config & Network" },
];

export function App() {
  const [tab, setTab] = useState<Tab>("services");
  const active = useAsync((s) => cli.getActive(s), [], 5000);
  const status = useAsync((s) => cli.getFlowStatus(s), [], 3000);
  const [busy, setBusy] = useState<string | null>(null);

  const control = async (name: string, action: () => Promise<unknown>) => {
    setBusy(name);
    try {
      await action();
      status.refresh();
    } catch (err) {
      alert(`${name} failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(null);
    }
  };

  const ready = status.data?.ready;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="brand-mark">codefly</span>
          <span className="brand-sub">dashboard</span>
        </div>
        <div className="active">
          {active.data?.workspace ? (
            <>
              <span className="active-ws">{active.data.workspace}</span>
              {active.data.service && (
                <span className="active-svc">
                  {active.data.module ? `${active.data.module} / ` : ""}
                  {active.data.service}
                </span>
              )}
            </>
          ) : (
            <span className="muted">no active workspace</span>
          )}
        </div>
        <div className="controls">
          <span className={`pill ${ready ? "pill-ok" : "pill-idle"}`}>
            {status.error ? "unreachable" : ready ? "flow ready" : "flow not ready"}
          </span>
          <button
            disabled={busy !== null}
            onClick={() => control("Stop flow", () => cli.stopFlow())}
          >
            {busy === "Stop flow" ? "Stopping…" : "Stop"}
          </button>
          <button
            className="danger"
            disabled={busy !== null}
            onClick={() => {
              if (confirm("Destroy the active flow and its resources?")) {
                control("Destroy flow", () => cli.destroyFlow());
              }
            }}
          >
            {busy === "Destroy flow" ? "Destroying…" : "Destroy"}
          </button>
        </div>
      </header>

      <nav className="tabs">
        {TABS.map((t) => (
          <button
            key={t.id}
            className={t.id === tab ? "tab tab-active" : "tab"}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </nav>

      <main className="content">
        {tab === "services" && <ServicesView active={active.data} ready={ready} />}
        {tab === "logs" && <LogsView />}
        {tab === "graph" && <GraphView />}
        {tab === "config" && <ConfigView />}
      </main>
    </div>
  );
}
