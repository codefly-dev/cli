import { cli, type Active, type Service } from "../api/cli";
import { useAsync } from "../hooks/useAsync";
import { Empty, ErrorBox, Loading } from "./common";

export function ServicesView({ active, ready }: { active: Active | null; ready?: boolean }) {
  const inventory = useAsync((s) => cli.getWorkspaceInventory(s), [], 5000);

  if (inventory.loading && !inventory.data) return <Loading />;
  if (inventory.error) return <ErrorBox error={inventory.error} />;

  const modules = inventory.data?.modules ?? [];
  if (modules.length === 0) return <Empty>No services in the active workspace.</Empty>;

  return (
    <div className="services">
      {modules.map((module) => (
        <section key={module.name} className="module-block">
          <h2 className="module-title">
            {module.name}
            {module.description && <span className="muted"> — {module.description}</span>}
          </h2>
          <div className="cards">
            {(module.services ?? []).map((service) => (
              <ServiceCard
                key={service.name}
                service={service}
                isActive={
                  active?.module === module.name && active?.service === service.name
                }
                ready={ready}
              />
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

function ServiceCard({
  service,
  isActive,
  ready,
}: {
  service: Service;
  isActive: boolean;
  ready?: boolean;
}) {
  const endpoints = service.endpoints ?? [];
  return (
    <article className={isActive ? "card card-active" : "card"}>
      <header className="card-head">
        <span className="card-name">{service.name}</span>
        {isActive && (
          <span className={`pill ${ready ? "pill-ok" : "pill-idle"}`}>
            {ready ? "running" : "starting"}
          </span>
        )}
      </header>
      {service.description && <p className="card-desc">{service.description}</p>}
      <dl className="card-meta">
        {service.agent?.name && (
          <div>
            <dt>agent</dt>
            <dd>
              {service.agent.name}
              {service.agent.version ? `@${service.agent.version}` : ""}
            </dd>
          </div>
        )}
        <div>
          <dt>endpoints</dt>
          <dd>{endpoints.length === 0 ? "—" : endpoints.length}</dd>
        </div>
      </dl>
    </article>
  );
}
