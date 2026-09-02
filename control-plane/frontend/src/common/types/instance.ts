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
  /** OpenBao access to named shared secret sets, beyond this agent's own always-RW namespace. */
  secret_grants: SecretGrant[];
  /** True once this agent has been minted an OpenBao token (feature on + provisioning completed). */
  has_openbao_access: boolean;
  /** Per-agent opt-in: mint an OpenConnector runtime token and register it as an MCP server (mcp.servers.open-connector). Off by default -- admin only. */
  connector_mcp_enabled: boolean;
  /** Per-agent opt-in: inject the shared connector's own admin bearer token into this agent's env (OOMOL_CONNECT_ADMIN_TOKEN). Bigger grant than connector_mcp_enabled -- admin only. */
  connector_admin_access_enabled: boolean;
  /** True once this agent has been minted an OpenConnector runtime token (connector_mcp_enabled + provisioning completed). */
  has_connector_access: boolean;
}

/** One shared OpenBao secret set grant: read-only or read+write access to secret/shared/<set_name>/*. */
export interface SecretGrant {
  set_name: string;
  capability: "read" | "write";
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
  secret_grants?: SecretGrant[];
  /** Opt this agent into the managed OpenConnector integration at creation time (admin only). */
  connector_mcp_enabled?: boolean;
  /** Opt this agent into having the connector's admin token injected into its env at creation time (admin only). */
  connector_admin_access_enabled?: boolean;
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
  /** Full replacement of this agent's shared-secret-set grants (admin only). */
  secret_grants?: SecretGrant[];
  /** Opt this agent into/out of the managed OpenConnector integration (admin only). */
  connector_mcp_enabled?: boolean;
  /** Opt this agent into/out of having the connector's admin token injected into its env (admin only). */
  connector_admin_access_enabled?: boolean;
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

/** Curated builtin-memory settings managed by Claworc (see docs/memory-config.md).
 * Maps to OpenClaw's memory.search.* config plus top-level memory.citations.
 * Unset fields inherit: instance override → global default → OpenClaw default. */
export interface MemorySettings {
  /** memory.search.provider: embedding adapter id ("openai", "gemini", "local",
   * "none" for FTS-only, or a custom models.providers.<id> key). */
  provider?: string;
  /** memory.search.model: embedding model name override. */
  model?: string;
  /** memory.search.fallback: adapter id tried when the primary provider fails. */
  fallback?: string;
  /** memory.search.query.maxResults (OpenClaw default 6). */
  max_results?: number;
  /** memory.search.query.minScore (0.0-1.0). */
  min_score?: number;
  /** Top-level memory.citations: "auto" | "on" | "off". */
  citations?: "auto" | "on" | "off";
  /** memory.search.rememberAcrossConversations: recall context from this
   * agent's other recognized private conversations. */
  remember_across_conversations?: boolean;
  /** Adds/removes "sessions" from memory.search.sources so session
   * transcripts are indexed and searchable. */
  sessions_enabled?: boolean;
  /** Raw JSON object deep-merged into memory.search for anything not modeled
   * above (multimodal, remote endpoint/headers, store.vector, cache, ...). */
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
  /** Per-instance override object. */
  settings: MemorySettings;
  /** Global defaults + override merged. */
  effective_settings: MemorySettings;
  indexed_folders: IndexedFolder[];
  restarts_gateway_on_apply: boolean;
}

export interface InstanceMemoryUpdatePayload {
  /** Full replacement of the per-instance override object. */
  settings: MemorySettings;
}

/** One lossless-claw fallbackProviders entry: an alternate provider/model
 * pair tried if the primary summary call fails. */
export interface LosslessClawFallbackProvider {
  provider: string;
  model: string;
}

/** Curated lossless-claw context-engine settings managed by Claworc.
 * Unset fields inherit: instance override -> global default -> plugin default.
 * See internal/handlers/contextengine.go's LosslessClawSettings for the
 * authoritative field list, sourced from `openclaw plugins inspect
 * lossless-claw --json`'s configJsonSchema/configUiHints. */
export interface LosslessClawSettings {
  /** Fraction of the context window (0-1) that triggers compaction. */
  context_threshold?: number;
  /** Number of recent messages protected from compaction. */
  fresh_tail_count?: number;
  /** Max source tokens per leaf compaction chunk before summarization. */
  leaf_chunk_tokens?: number;
  /** Preferred max condensation source depth during sweeps (0 = leaf only, -1 = unlimited). */
  sweep_max_depth?: number;
  host_fallback_mode?: "error" | "capture-only";
  /** Keep older context by prompt relevance instead of pure chronology under budget pressure. */
  prompt_aware_eviction?: boolean;
  /** Replace evicted large tool-result rows with an [LCM Tool Output: file_xxx] reference. */
  stub_large_tool_payloads?: boolean;
  /** Free-text guidance appended to the plugin's own compaction/recall behavior. */
  custom_instructions?: string;
  /** Model lossless-claw uses for its own summarization calls. Setting this
   * (or summary_provider) also grants plugins.entries.lossless-claw.llm.allowModelOverride. */
  summary_model?: string;
  summary_provider?: string;
  /** Alternate provider/model pairs tried if the primary summary call fails. */
  fallback_providers?: LosslessClawFallbackProvider[];
  /** Raw JSON object deep-merged into plugins.entries.lossless-claw.config for
   * anything not modeled above (contextThresholdOverrides, cacheAwareCompaction,
   * dynamicLeafChunkTokens, autoRotateSessionFiles, independentLogFile, ...). */
  advanced?: Record<string, unknown>;
}

/** GET/PATCH /instances/{id}/context-engine payload. */
export interface InstanceContextEngine {
  /** Per-instance override; "" = inherit the global default. */
  context_engine: "" | "legacy" | "lossless-claw";
  effective_engine: "legacy" | "lossless-claw";
  default_engine: "legacy" | "lossless-claw";
  lossless_claw: LosslessClawSettings;
  effective_lossless_claw: LosslessClawSettings;
  restarts_gateway_on_apply: boolean;
}

export interface InstanceContextEngineUpdatePayload {
  context_engine?: "" | "legacy" | "lossless-claw";
  /** Full replacement of the per-instance override object. */
  lossless_claw?: LosslessClawSettings;
}
