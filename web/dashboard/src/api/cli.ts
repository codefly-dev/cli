// Typed wrappers over the codefly.cli.v0.CLI Connect service. Types mirror the
// proto3 JSON shape (camelCase fields) of the messages defined in
// core/proto/codefly/{cli,observability,base}/v0. Only the fields the dashboard
// renders are declared; unused fields are intentionally omitted.

import { serverStream, unary } from "./connect";

export interface Active {
  workspace?: string;
  module?: string;
  service?: string;
}

export interface FlowStatus {
  ready?: boolean;
}

export interface Agent {
  kind?: string;
  name?: string;
  publisher?: string;
  version?: string;
}

export interface Endpoint {
  description?: string;
}

export interface Service {
  name?: string;
  description?: string;
  agent?: Agent;
  endpoints?: Endpoint[];
}

export interface Module {
  name?: string;
  description?: string;
  services?: Service[];
  serviceEntry?: string;
}

export interface Workspace {
  name?: string;
  description?: string;
  modules?: Module[];
  layout?: string;
}

export type GraphNodeType = "MODULE" | "SERVICE" | "ENDPOINT";

export interface GraphNode {
  id?: string;
  type?: GraphNodeType;
}

export interface GraphEdge {
  from?: string;
  to?: string;
}

export interface GraphResponse {
  nodes?: GraphNode[];
  edges?: GraphEdge[];
}

export interface Log {
  at?: string;
  module?: string;
  service?: string;
  kind?: string;
  message?: string;
}

export interface LogSessionGroup {
  logs?: Log[];
}

export interface LogResponse {
  groups?: LogSessionGroup[];
}

export interface ConfigurationValue {
  key?: string;
  value?: string;
  secret?: boolean;
}

export interface ConfigurationInformation {
  name?: string;
  configurationValues?: ConfigurationValue[];
}

export interface Configuration {
  origin?: string;
  infos?: ConfigurationInformation[];
}

export interface GetConfigurationsResponse {
  configurations?: Configuration[];
}

export interface NetworkInstance {
  access?: string;
  host?: string;
  hostname?: string;
  port?: number;
  address?: string;
}

export interface NetworkMapping {
  endpoint?: Endpoint;
  instances?: NetworkInstance[];
}

export interface GetNetworkMappingsResponse {
  networkMappings?: NetworkMapping[];
}

export interface ServiceRef {
  module: string;
  service: string;
}

export const cli = {
  getActive: (signal?: AbortSignal) => unary<Active>("GetActive", {}, signal),
  getFlowStatus: (signal?: AbortSignal) => unary<FlowStatus>("GetFlowStatus", {}, signal),
  getWorkspaceInventory: (signal?: AbortSignal) =>
    unary<Workspace>("GetWorkspaceInventory", {}, signal),
  getServiceDependencyGraph: (signal?: AbortSignal) =>
    unary<GraphResponse>("GetWorkspaceServiceDependencyGraph", {}, signal),
  activeLogHistory: (signal?: AbortSignal) =>
    unary<LogResponse>("ActiveLogHistory", {}, signal),
  getRuntimeConfigurations: (ref: ServiceRef, signal?: AbortSignal) =>
    unary<GetConfigurationsResponse>("GetRuntimeConfigurations", ref, signal),
  getDependenciesConfigurations: (ref: ServiceRef, signal?: AbortSignal) =>
    unary<GetConfigurationsResponse>("GetDependenciesConfigurations", ref, signal),
  getDependenciesNetworkMappings: (ref: ServiceRef, signal?: AbortSignal) =>
    unary<GetNetworkMappingsResponse>("GetDependenciesNetworkMappings", ref, signal),
  stopFlow: (signal?: AbortSignal) => unary<object>("StopFlow", {}, signal),
  destroyFlow: (signal?: AbortSignal) => unary<object>("DestroyFlow", {}, signal),
  streamLogs: (signal?: AbortSignal) => serverStream<Log>("Logs", {}, signal),
};
