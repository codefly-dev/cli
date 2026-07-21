import { useEffect, useMemo, useRef, useState } from "react";
import { cli, type Log } from "../api/cli";
import { useAsync } from "../hooks/useAsync";
import { ErrorBox, Loading } from "./common";

const MAX_LOGS = 3000;

export function LogsView() {
  const history = useAsync((s) => cli.activeLogHistory(s), []);
  const [live, setLive] = useState<Log[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [filter, setFilter] = useState<string>("");
  const [follow, setFollow] = useState(true);
  const bottomRef = useRef<HTMLDivElement>(null);

  // Live tail. The stream runs for the lifetime of the view; aborting on unmount
  // closes the underlying fetch so the server stops the log subscription.
  useEffect(() => {
    const controller = new AbortController();
    setStreaming(true);
    (async () => {
      try {
        for await (const log of cli.streamLogs(controller.signal)) {
          setLive((prev) => {
            const next = prev.length >= MAX_LOGS ? prev.slice(prev.length - MAX_LOGS + 1) : prev;
            return [...next, log];
          });
        }
      } catch {
        // stream ended or aborted; the history + connection pill convey state
      } finally {
        if (!controller.signal.aborted) setStreaming(false);
      }
    })();
    return () => controller.abort();
  }, []);

  const logs = useMemo(() => {
    const historic = history.data?.groups?.flatMap((g) => g.logs ?? []) ?? [];
    return [...historic, ...live];
  }, [history.data, live]);

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

  if (history.loading && !history.data) return <Loading />;
  if (history.error && logs.length === 0) return <ErrorBox error={history.error} />;

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
