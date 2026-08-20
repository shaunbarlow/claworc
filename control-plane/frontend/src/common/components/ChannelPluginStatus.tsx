import { CheckCircle2, AlertTriangle, HelpCircle, XCircle } from "lucide-react";
import type { ChannelPluginStatus as Status } from "@common/types/channelPlugin";

/**
 * Reports whether the OpenClaw plugin backing a channel actually loaded in the
 * agent, rather than leaving "enabled in Claworc" to stand in for it.
 *
 * "unknown" is styled as neutral, not as a warning: it means we could not ask
 * the agent (stopped, still booting, SSH down), which says nothing about
 * whether the plugin is healthy. Painting it amber would train people to
 * ignore the amber that matters.
 */
export default function ChannelPluginStatusLine({
  status,
  channelLabel,
}: {
  status?: Status;
  channelLabel: string;
}) {
  if (!status) return null;

  const detail = status.detail ? ` — ${status.detail}` : "";

  switch (status.state) {
    case "loaded":
      return (
        <p className="flex items-start gap-1.5 text-xs text-green-700">
          <CheckCircle2 size={14} className="mt-px shrink-0" />
          <span>The agent loaded the {channelLabel} plugin.</span>
        </p>
      );
    case "disabled":
      return (
        <p className="flex items-start gap-1.5 text-xs text-amber-700">
          <AlertTriangle size={14} className="mt-px shrink-0" />
          <span>
            The {channelLabel} plugin is installed but not running
            {status.detail ? (
              <>
                {" "}
                — OpenClaw reports <span className="font-medium">{status.detail}</span>
              </>
            ) : null}
            . The agent won't respond on {channelLabel} until that is resolved in its OpenClaw
            config. Claworc does not change <code>plugins.*</code> on your behalf, since an
            allowlist or denylist there is a deliberate choice.
          </span>
        </p>
      );
    case "error":
      return (
        <p className="flex items-start gap-1.5 text-xs text-red-600">
          <XCircle size={14} className="mt-px shrink-0" />
          <span>The {channelLabel} plugin failed to load{detail}</span>
        </p>
      );
    case "missing":
      return (
        <p className="flex items-start gap-1.5 text-xs text-red-600">
          <XCircle size={14} className="mt-px shrink-0" />
          <span>
            The agent has no {channelLabel} plugin. It normally ships inside OpenClaw itself, so
            this points at the agent image rather than at these settings.
          </span>
        </p>
      );
    default:
      return (
        <p className="flex items-start gap-1.5 text-xs text-gray-500">
          <HelpCircle size={14} className="mt-px shrink-0" />
          <span>Plugin status unavailable{detail}</span>
        </p>
      );
  }
}
