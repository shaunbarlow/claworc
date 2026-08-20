/**
 * Whether the OpenClaw plugin backing a chat channel actually loaded in the
 * agent. Returned by GET /instances/{id}/{slack,discord}, and only when the
 * channel is enabled — there is nothing to report otherwise.
 */
export interface ChannelPluginStatus {
  /**
   * - `loaded`   — running; the channel can work.
   * - `disabled` — present but switched off in the agent's OpenClaw config.
   * - `error`    — present but failed to load; `detail` carries the reason.
   * - `missing`  — not in the agent at all (an agent-image problem).
   * - `unknown`  — the agent could not be asked; says nothing about health.
   * - `checking` — a probe is running and no answer has landed yet.
   */
  state: "loaded" | "disabled" | "error" | "missing" | "unknown" | "checking";
  detail?: string;
}
