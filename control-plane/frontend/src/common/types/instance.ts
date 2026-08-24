import type { SlackCreateConfig } from "./slack";
import type { DiscordCreateConfig } from "./discord";

export interface InstanceModels {
  effective: string[];
  disabled_defaults: string[];
  extra: string[];
}

export interface Instance {
  id: number;
  name: string;
  display_name: string;
  status: "creating" | "running" | "restarting" | "stopping" | "stopped" | "error";
  status_message?: string;
  cpu_request: string;
  cpu_limit: string;
  memory_request: string;
  memory_limit: string;
  storage_homebrew: string;
  storage_home: string;
  has_brave_override: boolean;
  /** Per-instance search-provider override. "" = inherit the global default. */
  search_provider: "" | "brave";
  /** Resolved provider actually applied (instance override, else global default, else ""). */
  effective_search_provider: "" | "brave";
  models: InstanceModels;
  default_model: string;
  container_image: string | null;
  has_image_override: boolean;
  vnc_resolution: string | null;
  has_resolution_override: boolean;
  timezone: string | null;
  has_timezone_override: boolean;
  user_agent: string | null;
  has_user_agent_override: boolean;
  /** Per-instance env var overrides. Values are masked (e.g. "****abcd"). */
  env_vars: Record<string, string>;
  has_env_override: boolean;
  /** Set to true when env var changes were saved but a restart is needed to apply them. */
  requires_restart?: boolean;
  /** Set to true by the backend when it kicked off an auto-restart to apply env var changes. */
  restarting?: boolean;
  live_image_info?: string;
  allowed_source_ips: string;
  enabled_providers: number[];
  instance_providers: LLMProvider[];
  control_url: string;
  gateway_token: string;
  sort_order: number;
  created_at: string;
  updated_at: string;
  /** True when the instance still uses the combined image (browser baked into agent). */
  is_legacy_embedded: boolean;
  /** On-demand browser pod settings (only meaningful when !is_legacy_embedded). */
  browser_provider?: string;
  browser_image?: string;
  browser_idle_minutes?: number | null;
  browser_storage?: string;
  browser_active?: boolean;
  /** Hard per-agent gate: false means no browser pod may ever be spawned. */
  browser_enabled?: boolean;
  team_id: number;
  pod_annotations: Record<string, string>;
  node_selector: Record<string, string>;
  tolerations: Toleration[];
  affinity: string;
  service_account_annotations: Record<string, string>;
  ports: PortSpec[];
}

export interface PortSpec {
  name?: string;
  container_port: number;
  service_port?: number;
  protocol?: "TCP" | "UDP" | "";
}

// Keep as distinct type for future detail-only fields
export type InstanceDetail = Instance;

export interface InstanceCreatePayload {
  display_name: string;
  cpu_request?: string;
  cpu_limit?: string;
  memory_request?: string;
  memory_limit?: string;
  storage_homebrew?: string;
  storage_home?: string;
  brave_api_key?: string | null;
  search_provider?: "" | "brave";
  models?: { disabled: string[]; extra: string[] };
  default_model?: string;
  container_image?: string | null;
  vnc_resolution?: string | null;
  timezone?: string | null;
  user_agent?: string | null;
  enabled_providers?: number[];
  env_vars_set?: Record<string, string>;
  slack?: SlackCreateConfig;
  discord?: DiscordCreateConfig;
  browser_provider?: string;
  browser_image?: string;
  browser_idle_minutes?: number;
  browser_storage?: string;
  browser_enabled?: boolean;
  team_id?: number;
  pod_annotations?: Record<string, string>;
  node_selector?: Record<string, string>;
  tolerations?: Toleration[];
  affinity?: string;
  service_account_annotations?: Record<string, string>;
  ports?: PortSpec[];
}

export interface Toleration {
  key?: string;
  operator: "Equal" | "Exists";
  value?: string;
  effect?: "NoSchedule" | "PreferNoSchedule" | "NoExecute" | "";
  tolerationSeconds?: number;
}

export interface InstanceUpdatePayload {
  brave_api_key?: string;
  search_provider?: "" | "brave";
  models?: { disabled: string[]; extra: string[] };
  default_model?: string;
  timezone?: string;
  user_agent?: string;
  allowed_source_ips?: string;
  enabled_providers?: number[];
  display_name?: string;
  cpu_request?: string;
  cpu_limit?: string;
  memory_request?: string;
  memory_limit?: string;
  vnc_resolution?: string;
  env_vars_set?: Record<string, string>;
  env_vars_unset?: string[];
  browser_provider?: string;
  browser_image?: string;
  browser_idle_minutes?: number | null;
  browser_storage?: string;
  team_id?: number;
  pod_annotations?: Record<string, string>;
  node_selector?: Record<string, string>;
  tolerations?: Toleration[];
  affinity?: string;
  service_account_annotations?: Record<string, string>;
  ports?: PortSpec[];
}

export interface InstanceStats {
  cpu_usage_millicores: number;
  cpu_usage_percent: number;
  memory_usage_bytes: number;
  memory_limit_bytes: number;
}

export interface ProviderModelCost {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export interface ProviderModel {
  id: string;
  name: string;
  reasoning?: boolean;
  input?: string[];
  contextWindow?: number;
  maxTokens?: number;
  cost?: ProviderModelCost;
}

export interface LLMProvider {
  id: number;
  key: string;
  instance_id?: number; // non-null = instance-specific provider
  provider: string; // catalog provider key, empty for custom
  name: string;
  base_url: string;
  api_type: string;
  masked_api_key?: string;
  models: ProviderModel[] | null;
  oauth_connected?: boolean;
  oauth_email?: string;
  oauth_expires_at?: number;
  created_at: string;
  updated_at: string;
}

export interface InstanceConfig {
  config: string;
}

export interface InstanceConfigUpdate {
  config: string;
  restarted: boolean;
}

/** One memory.qmd.scope rule: allow/deny gated on chat type. Renders to
 * OpenClaw's `{action, match: {chatType}}` shape in buildMemoryConfig. */
export interface MemoryQmdScopeRule {
  action: "allow" | "deny";
  chat_type: "direct" | "group" | "channel";
}

/** Which chat types can see QMD search results (memory.qmd.scope). OpenClaw's
 * own default is `{default: "deny", rules: [{action: "allow", match: {chatType: "direct"}}]}` —
 * i.e. direct chats only. */
export interface MemoryQmdScope {
  default?: "allow" | "deny";
  rules?: MemoryQmdScopeRule[];
}

/** Curated QMD memory settings managed by Claworc (see docs/qmd-memory.md).
 * Unset fields inherit: instance override → global default → OpenClaw default. */
export interface MemoryQmdSettings {
  search_mode?: "search" | "vsearch" | "query";
  /** Reindex cadence, e.g. "5m", "1h". */
  update_interval?: string;
  max_results?: number;
  /** Index session transcripts. */
  sessions_enabled?: boolean;
  /** Index MEMORY.md and memory/ markdown in the workspace. */
  include_default_memory?: boolean;
  /** Which chat types (direct/group/channel) can see QMD search results.
   * Unset = inherit; OpenClaw's own default only allows direct chats. */
  scope?: MemoryQmdScope;
  /** Raw JSON object deep-merged into memory.qmd for anything not modeled above. */
  advanced?: Record<string, unknown>;
}

export interface IndexedFolder {
  id: number;
  name: string;
  mount_path: string;
  pattern: string;
}

/** GET/PATCH /instances/{id}/memory payload. */
export interface InstanceMemory {
  /** Per-instance override; "" = inherit the global default. */
  memory_backend: "" | "builtin" | "qmd";
  effective_backend: "builtin" | "qmd";
  default_backend: "builtin" | "qmd";
  qmd: MemoryQmdSettings;
  effective_qmd: MemoryQmdSettings;
  indexed_folders: IndexedFolder[];
  restarts_gateway_on_apply: boolean;
}

export interface InstanceMemoryUpdatePayload {
  memory_backend?: "" | "builtin" | "qmd";
  /** Full replacement of the per-instance override object. */
  qmd?: MemoryQmdSettings;
}
