import { useEffect, useRef, useState } from "react";
import type { SecretGrant } from "@common/types/instance";

interface Props {
  /** Every configured shared secret set's name (from settings.openbao_shared_sets). */
  availableSets: string[];
  grants: SecretGrant[];
  /** Required in managed mode (the default). */
  onSave?: (next: SecretGrant[]) => Promise<void> | void;
  isSaving?: boolean;
  /**
   * When true, render the edit grid permanently (no display/edit toggle, no
   * Save/Cancel buttons) and report the current grants via onChange. Used by
   * AgentForm, which owns the submit action. Mirrors SimpleKVEditor/
   * EnvVarsEditor's inline mode.
   */
  inline?: boolean;
  /** Inline-mode change callback; fires whenever the grant list changes. */
  onChange?: (next: SecretGrant[]) => void;
}

type Level = "none" | "read" | "write";

function grantsToLevels(availableSets: string[], grants: SecretGrant[]): Record<string, Level> {
  const byName = new Map(grants.map((g) => [g.set_name, g.capability] as const));
  const levels: Record<string, Level> = {};
  for (const name of availableSets) {
    levels[name] = (byName.get(name) as Level) ?? "none";
  }
  // Preserve grants pointing at sets that no longer exist in availableSets
  // (e.g. the set was deleted from Settings after this agent was granted
  // access to it) so an admin editing this agent doesn't silently drop them
  // without noticing.
  for (const g of grants) {
    if (!(g.set_name in levels)) {
      levels[g.set_name] = g.capability as Level;
    }
  }
  return levels;
}

function levelsToGrants(levels: Record<string, Level>): SecretGrant[] {
  return Object.entries(levels)
    .filter(([, level]) => level !== "none")
    .map(([set_name, level]) => ({ set_name, capability: level as "read" | "write" }));
}

export default function SecretGrantsEditor({
  availableSets,
  grants,
  onSave,
  isSaving,
  inline = false,
  onChange,
}: Props) {
  const [editing, setEditing] = useState(inline);
  const [levels, setLevels] = useState<Record<string, Level>>(() =>
    inline ? grantsToLevels(availableSets, grants) : {},
  );

  const lastEmitRef = useRef<string>("");
  useEffect(() => {
    if (!inline || !onChange) return;
    const next = levelsToGrants(levels);
    const serialized = JSON.stringify(next);
    if (serialized !== lastEmitRef.current) {
      lastEmitRef.current = serialized;
      onChange(next);
    }
  }, [levels, inline, onChange]);

  const beginEdit = () => {
    setLevels(grantsToLevels(availableSets, grants));
    setEditing(true);
  };

  const cancel = () => {
    setEditing(false);
    setLevels({});
  };

  const handleSave = async () => {
    if (!onSave) return;
    await onSave(levelsToGrants(levels));
    setEditing(false);
    setLevels({});
  };

  // Rows come from whichever set of names is relevant right now: every
  // configured set, plus (in edit mode) any dangling grant not in that list.
  const rowNames = editing
    ? Object.keys(levels).sort((a, b) => a.localeCompare(b))
    : [...availableSets].sort((a, b) => a.localeCompare(b));

  return (
    <div className="bg-white rounded-lg border border-gray-200 p-6">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-medium text-gray-900">Secret access (OpenBao)</h3>
        {!inline && !editing && (
          <button type="button" onClick={beginEdit} className="text-xs text-blue-600 hover:text-blue-800">
            Edit
          </button>
        )}
      </div>
      <p className="text-xs text-gray-500 mb-4">
        This agent always has full read/write access to its own OpenBao secret path. Grant it
        read-only or read/write access to admin-managed shared secret sets below.
      </p>

      {!editing ? (
        availableSets.length === 0 ? (
          <p className="text-sm text-gray-400 italic">
            No shared secret sets configured yet — create one in Settings first.
          </p>
        ) : grants.length === 0 ? (
          <p className="text-sm text-gray-400 italic">No shared secret access granted.</p>
        ) : (
          <div className="divide-y divide-gray-100">
            {grants.map((g) => (
              <div key={g.set_name} className="py-2 flex items-center justify-between gap-4">
                <span className="text-sm font-mono text-gray-900">{g.set_name}</span>
                <span
                  className={`text-xs font-medium ${g.capability === "write" ? "text-amber-700" : "text-gray-500"}`}
                >
                  {g.capability === "write" ? "read + write" : "read only"}
                </span>
              </div>
            ))}
          </div>
        )
      ) : rowNames.length === 0 ? (
        <p className="text-sm text-gray-400 italic">
          No shared secret sets configured yet — create one in Settings first.
        </p>
      ) : (
        <div>
          <div className="space-y-2">
            {rowNames.map((name) => (
              <div key={name} className="flex items-center justify-between gap-4">
                <span className="text-sm font-mono text-gray-900">
                  {name}
                  {!availableSets.includes(name) && (
                    <span className="ml-2 text-[11px] text-amber-600">(set no longer exists)</span>
                  )}
                </span>
                <select
                  value={levels[name] ?? "none"}
                  onChange={(e) => setLevels((prev) => ({ ...prev, [name]: e.target.value as Level }))}
                  className="px-3 py-1.5 border border-gray-300 rounded-md text-sm bg-white focus:outline-none focus:ring-2 focus:ring-blue-500"
                >
                  <option value="none">No access</option>
                  <option value="read">Read only</option>
                  <option value="write">Read + write</option>
                </select>
              </div>
            ))}
          </div>

          {!inline && (
            <div className="flex justify-end gap-3 mt-4">
              <button
                type="button"
                onClick={cancel}
                disabled={isSaving}
                className="px-4 py-2 text-sm font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={handleSave}
                disabled={isSaving}
                className="px-4 py-2 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {isSaving ? "Saving..." : "Save"}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
