import { useState } from "react";
import { Loader2, Settings, Trash2 } from "lucide-react";
import {
  useDisableInstancePlugin,
  useEnableInstancePlugin,
  useInstallInstancePlugin,
  useInstancePlugins,
  useUninstallInstancePlugin,
} from "@common/hooks/useInstancePlugins";
import type { PluginSummary } from "@common/types/plugins";
import PluginConfigModal from "@common/components/plugins/PluginConfigModal";

interface Props {
  instanceId: number;
}

const STATUS_STYLES: Record<string, string> = {
  loaded: "bg-green-100 text-green-700",
  disabled: "bg-gray-100 text-gray-600",
  error: "bg-red-100 text-red-700",
};

function StatusBadge({ plugin }: { plugin: PluginSummary }) {
  const cls = STATUS_STYLES[plugin.status] ?? "bg-gray-100 text-gray-600";
  return (
    <span className={`text-[11px] px-2 py-0.5 rounded-full font-medium ${cls}`} title={plugin.error}>
      {plugin.status}
    </span>
  );
}

/**
 * Per-agent plugin manager: generalizes the Discord/Slack install+status
 * machinery to any OpenClaw plugin (npm spec, clawhub:<package>, or git
 * URL) -- e.g. @martian-engineering/lossless-claw, which is not and likely
 * will never be in OpenClaw's own channel catalog and so has no dedicated
 * settings card the way Slack/Discord do.
 *
 * Channel plugins (discord/slack) are still shown here for visibility but
 * point back at their dedicated cards rather than duplicating enable/disable
 * controls that would fight with those cards' own config-driven auto-enable.
 */
export default function PluginsSection({ instanceId }: Props) {
  const { data, isLoading, error } = useInstancePlugins(instanceId);
  const installMut = useInstallInstancePlugin(instanceId);
  const enableMut = useEnableInstancePlugin(instanceId);
  const disableMut = useDisableInstancePlugin(instanceId);
  const uninstallMut = useUninstallInstancePlugin(instanceId);

  const [spec, setSpec] = useState("");
  const [configPluginId, setConfigPluginId] = useState<string | null>(null);

  const handleInstall = () => {
    const trimmed = spec.trim();
    if (!trimmed) return;
    installMut.mutate(trimmed, { onSuccess: () => setSpec("") });
  };

  const handleUninstall = (pluginId: string) => {
    if (!confirm(`Uninstall plugin "${pluginId}"? This removes it from the agent.`)) return;
    uninstallMut.mutate(pluginId);
  };

  if (error) {
    return (
      <div className="bg-white rounded-lg border border-gray-200 p-6">
        <h3 className="text-sm font-medium text-gray-900">Plugins</h3>
        <p className="text-xs text-red-600 mt-2">Failed to load plugins.</p>
      </div>
    );
  }

  const plugins = data?.plugins ?? [];
  const nonChannelPlugins = plugins.filter((p) => !p.channel_ids || p.channel_ids.length === 0);
  const channelPlugins = plugins.filter((p) => p.channel_ids && p.channel_ids.length > 0);

  return (
    <div className="bg-white rounded-lg border border-gray-200 p-6">
      <h3 className="text-sm font-medium text-gray-900 mb-1">Plugins</h3>
      <p className="text-xs text-gray-500 mb-4">
        Install any OpenClaw plugin by npm package, <code>clawhub:&lt;package&gt;</code>{" "}
        reference, or git URL — e.g. lossless-claw
        (<code>@martian-engineering/lossless-claw</code>). Slack and Discord have their own
        cards above; they are listed here for visibility only.
      </p>

      <div className="flex items-center gap-2 mb-4">
        <input
          type="text"
          value={spec}
          onChange={(e) => setSpec(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") handleInstall();
          }}
          placeholder="@martian-engineering/lossless-claw, clawhub:owner/skill, or a git URL"
          className="flex-1 px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        <button
          type="button"
          onClick={handleInstall}
          disabled={!spec.trim() || installMut.isPending}
          className="px-4 py-1.5 text-xs font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
        >
          {installMut.isPending ? "Starting…" : "Install"}
        </button>
      </div>
      <p className="text-[11px] text-gray-500 mb-4">
        Installing runs <code>npm install</code> inside the agent and can take a few minutes —
        the list below refreshes once it finishes.
      </p>

      {isLoading || data?.state === "checking" ? (
        <div className="flex items-center gap-2 py-8 justify-center text-gray-400">
          <Loader2 size={16} className="animate-spin" />
          <span className="text-xs">Asking the agent…</span>
        </div>
      ) : data?.state === "unknown" ? (
        <p className="text-xs text-amber-700 py-4">
          Could not confirm the agent's plugin state{data.detail ? `: ${data.detail}` : "."}
        </p>
      ) : nonChannelPlugins.length === 0 && channelPlugins.length === 0 ? (
        <p className="text-xs text-gray-400 py-4">No plugins discovered on this agent.</p>
      ) : (
        <div className="space-y-1.5">
          {nonChannelPlugins.map((p) => (
            <div
              key={p.id}
              className="flex items-center gap-3 px-3 py-2 border border-gray-100 rounded-md"
            >
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium text-gray-900 truncate">{p.name}</span>
                  <span className="text-xs text-gray-400 font-mono">{p.id}</span>
                  {p.version && <span className="text-xs text-gray-400">v{p.version}</span>}
                </div>
                {p.error && <p className="text-[11px] text-gray-500 mt-0.5 truncate">{p.error}</p>}
              </div>
              <StatusBadge plugin={p} />
              {p.enabled ? (
                <button
                  type="button"
                  onClick={() => disableMut.mutate(p.id)}
                  disabled={disableMut.isPending}
                  className="px-2.5 py-1 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-50"
                >
                  Disable
                </button>
              ) : (
                <button
                  type="button"
                  onClick={() => enableMut.mutate(p.id)}
                  disabled={enableMut.isPending}
                  className="px-2.5 py-1 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-50"
                >
                  Enable
                </button>
              )}
              {p.config_schema && (
                <button
                  type="button"
                  title="Edit config"
                  onClick={() => setConfigPluginId(p.id)}
                  className="p-1.5 text-gray-500 hover:text-gray-800"
                >
                  <Settings size={14} />
                </button>
              )}
              <button
                type="button"
                title="Uninstall"
                onClick={() => handleUninstall(p.id)}
                disabled={uninstallMut.isPending}
                className="p-1.5 text-gray-500 hover:text-red-600 disabled:opacity-40"
              >
                <Trash2 size={14} />
              </button>
            </div>
          ))}

          {channelPlugins.length > 0 && (
            <div className="pt-2 mt-2 border-t border-gray-100">
              <p className="text-[11px] text-gray-400 mb-1.5">
                Channel plugins — managed from their own cards above:
              </p>
              {channelPlugins.map((p) => (
                <div
                  key={p.id}
                  className="flex items-center gap-3 px-3 py-1.5 text-gray-500"
                >
                  <span className="text-xs font-mono flex-1">{p.id}</span>
                  <StatusBadge plugin={p} />
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {configPluginId && (
        <PluginConfigModal
          instanceId={instanceId}
          pluginId={configPluginId}
          onClose={() => setConfigPluginId(null)}
        />
      )}
    </div>
  );
}
