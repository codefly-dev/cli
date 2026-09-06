import { useEffect, useMemo, useRef, useState } from "react";
import { cli, type Log } from "../api/cli";
import { ErrorBox, Loading } from "./common";

const MAX_LOGS = 3000;

export function LogsView() {
  const [logs, setLogs] = useState<Log[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [connecting, setConnecting] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const [filter, setFilter] = useState<string>("");
  const [follow, setFollow] = useState(true);
  const bottomRef = useRef<HTMLDivElement>(null);

  // Logs() replays the full backend history before switching to a live tail
  // (see pkg/web/go-grpc/server.go), all as one ordered stream, so this is
  // the single source of both backfill and live lines — it must not be
  // combined with a separate history fetch, which would double-count any
  // line recorded in the race window between two independent requests.
  // The stream runs for the lifetime of the view; aborting on unmount closes
  // the underlying fetch so the server stops the subscription.
  useEffect(() => {
    const controller = new AbortController();
    setStreaming(true);
    setConnecting(true);
    (async () => {
      try {
        for await (const log of cli.streamLogs(controller.signal, () => setConnecting(false))) {
          setLogs((prev) => {
            const next = prev.length >= MAX_LOGS ? prev.slice(prev.length - MAX_LOGS + 1) : prev;
            return [...next, log];
          });
        }
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err : new Error(String(err)));
        }
      } finally {
        setConnecting(false);
        if (!controller.signal.aborted) setStreaming(false);
      }
    })();
    return () => controller.abort();
  }, []);

  const services = useMemo(() => {
    const set = new Set<string>();
    for (const log of logs) {
      if (log.service) set.add(log.service);
    }
    return [...set].sort();
  }, [logs]);

  const shown = filter ? logs.filter((l) => l.service === filter) : logs;

  useEffect(() => {
    if (follow) bottomRef.current?.scrollIntoView();
  }, [shown.length, follow]);

  if (connecting) return <Loading />;
  if (error && logs.length === 0) return <ErrorBox error={error} />;

  return (
    <div className="logs">
      <div className="logs-toolbar">
        <select value={filter} onChange={(e) => setFilter(e.target.value)}>
          <option value="">all services</option>
          {services.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <label className="follow">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
          />
          follow
        </label>
        <span className={`pill ${streaming ? "pill-ok" : "pill-idle"}`}>
          {streaming ? "streaming" : "idle"}
        </span>
        <span className="muted logs-count">{shown.length} lines</span>
      </div>
      <div className="log-lines" onWheel={() => setFollow(false)}>
        {shown.map((log, i) => (
          <LogLine key={i} log={log} />
        ))}
        <div ref={bottomRef} />
      </div>
    </div>
  );
}

function LogLine({ log }: { log: Log }) {
  const time = log.at ? new Date(log.at).toLocaleTimeString() : "";
  return (
    <div className={`log-line kind-${(log.kind ?? "info").toLowerCase()}`}>
      <span className="log-time">{time}</span>
      <span className="log-svc">{log.service || log.module || "—"}</span>
      <span className="log-msg">{log.message}</span>
    </div>
  );
}
