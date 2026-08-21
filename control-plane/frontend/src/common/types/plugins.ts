export interface PluginSummary {
  id: string;
  name: string;
  version?: string;
  origin?: string;
  status: "loaded" | "disabled" | "error" | string;
  enabled: boolean;
  error?: string;
  config_schema: boolean;
  /** Non-empty for channel plugins (discord/slack/...) -- point the operator
   * at the dedicated Slack/Discord cards for those instead of this one. */
  channel_ids?: string[];
}

/** GET /instances/{id}/plugins response. */
export interface InstancePluginsList {
  plugins: PluginSummary[];
  state: "ok" | "checking" | "unknown";
  detail?: string;
}

export interface PluginActionResult {
  ok: boolean;
  error?: string;
  restarting?: boolean;
}

export interface InstallPluginResponse {
  task_id?: string;
}

export interface PluginConfigResponse {
  config: string;
}
