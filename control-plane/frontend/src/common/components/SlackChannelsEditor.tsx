import { Plus, Trash2 } from "lucide-react";
import type { SlackChannel } from "@common/types/slack";

interface Props {
  channels: SlackChannel[];
  onChange: (channels: SlackChannel[]) => void;
  disabled?: boolean;
}

/**
 * Controlled editor for the Slack channel allowlist: one row per channel ID
 * plus a per-channel "require @-mention" toggle. Shared by the agent
 * settings card and the create-agent form.
 */
export default function SlackChannelsEditor({ channels, onChange, disabled }: Props) {
  const patch = (idx: number, p: Partial<SlackChannel>) => {
    onChange(channels.map((c, i) => (i === idx ? { ...c, ...p } : c)));
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-1">
        <label className="block text-xs font-medium text-gray-700">Allowed channels</label>
        <button
          type="button"
          disabled={disabled}
          onClick={() => onChange([...channels, { id: "" }])}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-40"
        >
          <Plus size={12} />
          Add channel
        </button>
      </div>
      <p className="text-[11px] text-gray-500 mb-2">
        Use channel <span className="font-medium">IDs</span> (e.g. C0123456789), not names — copy
        the ID from the channel's "About" tab in Slack. The agent only responds in listed
        channels.
      </p>
      {channels.length === 0 ? (
        <p className="text-xs text-amber-700">
          No channels listed — the agent won't respond anywhere in Slack (DMs excepted).
        </p>
      ) : (
        <div className="space-y-1.5">
          {channels.map((ch, idx) => (
            <div key={idx} className="flex items-center gap-2">
              <input
                type="text"
                value={ch.id}
                disabled={disabled}
                onChange={(e) => patch(idx, { id: e.target.value })}
                placeholder="C0123456789"
                className="flex-1 px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:bg-gray-50 disabled:text-gray-400"
              />
              <label
                className="flex items-center gap-1.5 text-xs text-gray-700 select-none whitespace-nowrap"
                title="When checked, the agent only responds if @-mentioned in this channel"
              >
                <input
                  type="checkbox"
                  disabled={disabled}
                  checked={ch.require_mention !== false}
                  onChange={(e) => patch(idx, { require_mention: e.target.checked })}
                  className="h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                />
                Require @-mention
              </label>
              <button
                type="button"
                title="Remove channel"
                disabled={disabled}
                onClick={() => onChange(channels.filter((_, i) => i !== idx))}
                className="p-1.5 text-gray-500 hover:text-red-600 disabled:opacity-40"
              >
                <Trash2 size={14} />
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/**
 * DM access control. The "allowlist" policy names specific Slack users who
 * are answered straight away with no pairing handshake, so its user list is
 * part of the same control rather than a separate card — picking the policy
 * without naming anyone would block every DM, which the API rejects.
 */
export function SlackDMPolicySelect({
  value,
  onChange,
  allowFrom,
  onAllowFromChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  allowFrom: string[];
  onAllowFromChange: (users: string[]) => void;
  disabled?: boolean;
}) {
  const isAllowlist = value === "allowlist";
  return (
    <div>
      <label className="block text-xs font-medium text-gray-700 mb-1">Direct messages</label>
      <select
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm bg-white focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:bg-gray-50 disabled:text-gray-400"
      >
        <option value="">Pairing (default) — DMs require one-time approval</option>
        <option value="allowlist">
          Allowlist — only the listed users, answered without pairing
        </option>
        <option value="open">Open — anyone in the workspace can DM the agent</option>
        <option value="disabled">Disabled — the agent ignores DMs</option>
      </select>

      {isAllowlist && (
        <div className="mt-2 pl-3 border-l-2 border-gray-200">
          <div className="flex items-center justify-between mb-1">
            <label className="block text-xs font-medium text-gray-700">Allowed users</label>
            <button
              type="button"
              disabled={disabled}
              onClick={() => onAllowFromChange([...allowFrom, ""])}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-40"
            >
              <Plus size={12} />
              Add user
            </button>
          </div>
          <p className="text-[11px] text-gray-500 mb-2">
            Use Slack <span className="font-medium">member IDs</span> (click a user → View full
            profile → ⋮ → Copy member ID). These users skip pairing entirely; everyone else is
            ignored.
          </p>
          {allowFrom.length === 0 ? (
            <p className="text-xs text-amber-700">
              No users listed — add at least one, or switch to Disabled to block all DMs.
            </p>
          ) : (
            <div className="space-y-1.5">
              {allowFrom.map((uid, idx) => (
                <div key={idx} className="flex items-center gap-2">
                  <input
                    type="text"
                    value={uid}
                    disabled={disabled}
                    onChange={(e) =>
                      onAllowFromChange(allowFrom.map((u, i) => (i === idx ? e.target.value : u)))
                    }
                    placeholder="Member ID, e.g. U0123456789"
                    className="flex-1 px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:bg-gray-50 disabled:text-gray-400"
                  />
                  <button
                    type="button"
                    title="Remove user"
                    disabled={disabled}
                    onClick={() => onAllowFromChange(allowFrom.filter((_, i) => i !== idx))}
                    className="p-1.5 text-gray-500 hover:text-red-600 disabled:opacity-40"
                  >
                    <Trash2 size={14} />
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
