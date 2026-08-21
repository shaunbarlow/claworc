import type { ChannelPluginStatus } from "@common/types/channelPlugin";

export interface SlackChannel {
  /** Raw Slack channel ID (e.g. C0123456789) — not the channel name. */
  id: string;
  /** Undefined means OpenClaw's default (true): respond only when @-mentioned. */
  require_mention?: boolean;
}

/** GET /instances/{id}/slack response. */
export interface InstanceSlack {
  configured: boolean;
  enabled: boolean;
  channels: SlackChannel[];
  dm_policy: string;
  /** Slack member IDs allowed to DM the agent under the "allowlist" policy. */
  dm_allow_from: string[];
  /** "" (default, false) or "true". */
  allow_bots: string;
  has_bot_token: boolean;
  has_app_token: boolean;
  /** Masked token for display (e.g. "****abcd"), only when a token is set. */
  bot_token_masked?: string;
  app_token_masked?: string;
  /** Set by PUT when a token change triggered a container restart. */
  restarting?: boolean;
  /** Set by GET only, and only while the channel is enabled. */
  plugin_status?: ChannelPluginStatus;
}

/** PUT /instances/{id}/slack payload. Omitted fields keep their current value. */
export interface InstanceSlackUpdatePayload {
  enabled?: boolean;
  channels?: SlackChannel[];
  dm_policy?: string;
  dm_allow_from?: string[];
  allow_bots?: string;
  /** Tokens: omit = keep, "" = remove, non-empty = set. */
  bot_token?: string;
  app_token?: string;
}

/** Create-time Slack config embedded in InstanceCreatePayload. */
export interface SlackCreateConfig {
  enabled: boolean;
  channels?: SlackChannel[];
  dm_policy?: string;
  dm_allow_from?: string[];
  allow_bots?: string;
  bot_token?: string;
  app_token?: string;
}
