import { useMemo, useState, type ReactNode } from "react";
import { cli, type Configuration, type NetworkMapping, type ServiceRef } from "../api/cli";
import { useAsync } from "../hooks/useAsync";
import { Empty, ErrorBox, Loading } from "./common";

export function ConfigView() {
  const inventory = useAsync((s) => cli.getWorkspaceInventory(s), []);
  const [selected, setSelected] = useState<string>("");

  const refs = useMemo<ServiceRef[]>(() => {
    const out: ServiceRef[] = [];
    for (const module of inventory.data?.modules ?? []) {
      for (const service of module.services ?? []) {
        if (module.name && service.name) out.push({ module: module.name, service: service.name });
      }
    }
    return out;
  }, [inventory.data]);

  const current = refs.find((r) => key(r) === selected) ?? refs[0];

  if (inventory.loading && !inventory.data) return <Loading />;
  if (inventory.error) return <ErrorBox error={inventory.error} />;
  if (!current) return <Empty>No services to inspect.</Empty>;

  return (
    <div className="config">
      <div className="config-toolbar">
        <label>Service</label>
        <select value={key(current)} onChange={(e) => setSelected(e.target.value)}>
          {refs.map((r) => (
            <option key={key(r)} value={key(r)}>
              {r.module} / {r.service}
            </option>
          ))}
        </select>
      </div>
      <ServiceConfig ref_={current} />
    </div>
  );
}

function ServiceConfig({ ref_ }: { ref_: ServiceRef }) {
  const runtime = useAsync((s) => cli.getRuntimeConfigurations(ref_, s), [key(ref_)]);
  const deps = useAsync((s) => cli.getDependenciesConfigurations(ref_, s), [key(ref_)]);
  const network = useAsync((s) => cli.getDependenciesNetworkMappings(ref_, s), [key(ref_)]);

  return (
    <div className="config-panels">
      <Panel title="Runtime configuration">
        <ConfigList configs={runtime.data?.configurations} error={runtime.error} />
      </Panel>
      <Panel title="Dependency configuration">
        <ConfigList configs={deps.data?.configurations} error={deps.error} />
      </Panel>
      <Panel title="Network mappings">
        <NetworkList mappings={network.data?.networkMappings} error={network.error} />
      </Panel>
    </div>
  );
}

function Panel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="panel">
      <h3 className="panel-title">{title}</h3>
      {children}
    </section>
  );
}

function ConfigList({ configs, error }: { configs?: Configuration[]; error: Error | null }) {
  if (error) return <div className="muted">{error.message}</div>;
  if (!configs || configs.length === 0) return <div className="muted">none</div>;
  return (
    <>
      {configs.map((config, i) => (
        <div key={i} className="config-group">
          <div className="config-origin">{config.origin || "config"}</div>
          {(config.infos ?? []).map((info, j) => (
            <table key={j} className="kv">
              <tbody>
                {(info.configurationValues ?? []).map((v, k) => (
                  <tr key={k}>
                    <td className="kv-key">{v.key}</td>
                    <td className="kv-val">{v.secret ? "••••••" : v.value}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ))}
        </div>
      ))}
    </>
  );
}

function NetworkList({ mappings, error }: { mappings?: NetworkMapping[]; error: Error | null }) {
  if (error) return <div className="muted">{error.message}</div>;
  if (!mappings || mappings.length === 0) return <div className="muted">none</div>;
  return (
    <table className="kv">
      <tbody>
        {mappings.flatMap((mapping, i) =>
          (mapping.instances ?? []).map((instance, j) => (
            <tr key={`${i}-${j}`}>
              <td className="kv-key">{instance.access || "—"}</td>
              <td className="kv-val">{instance.address || `${instance.host}:${instance.port}`}</td>
            </tr>
          )),
        )}
      </tbody>
    </table>
  );
}

function key(ref: ServiceRef): string {
  return `${ref.module}/${ref.service}`;
}
