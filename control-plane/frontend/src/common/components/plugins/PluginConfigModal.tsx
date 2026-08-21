import { useEffect, useState } from "react";
import { X } from "lucide-react";
import MonacoConfigEditor from "@common/components/MonacoConfigEditor";
import { useInstancePluginConfig, useSaveInstancePluginConfig } from "@common/hooks/useInstancePlugins";

interface Props {
  instanceId: number;
  pluginId: string;
  onClose: () => void;
}

/**
 * Raw-JSON editor for `plugins.entries.<id>.config`. There is no generic
 * per-plugin schema Claworc can render a form from -- OpenClaw's own
 * `configSchema` flag on a plugin entry only says whether one exists, not
 * what it looks like -- so this is a JSON textarea, the same tradeoff the
 * skill file editor makes for SKILL.md.
 */
export default function PluginConfigModal({ instanceId, pluginId, onClose }: Props) {
  const { data, isLoading } = useInstancePluginConfig(instanceId, pluginId);
  const save = useSaveInstancePluginConfig(instanceId, pluginId);
  const [value, setValue] = useState("{}");
  const [dirty, setDirty] = useState(false);
  const [jsonError, setJsonError] = useState<string | null>(null);

  useEffect(() => {
    if (data && !dirty) {
      try {
        setValue(JSON.stringify(JSON.parse(data.config), null, 2));
      } catch {
        setValue(data.config);
      }
    }
  }, [data, dirty]);

  const handleChange = (v: string | undefined) => {
    setValue(v ?? "");
    setDirty(true);
    try {
      JSON.parse(v ?? "");
      setJsonError(null);
    } catch (e) {
      setJsonError((e as Error).message);
    }
  };

  const handleSave = () => {
    if (jsonError) return;
    save.mutate(value, {
      onSuccess: (res) => {
        if (res.ok) {
          setDirty(false);
          onClose();
        }
      },
    });
  };

  const closeWithConfirm = () => {
    if (dirty && !confirm("Discard unsaved config changes?")) return;
    onClose();
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
      <div className="bg-white rounded-xl shadow-xl w-full max-w-2xl mx-4 flex flex-col max-h-[80vh]">
        <div className="flex items-center justify-between px-6 py-4 border-b border-gray-200">
          <div>
            <h2 className="text-base font-semibold text-gray-900">Plugin config</h2>
            <p className="text-xs text-gray-500 font-mono mt-0.5">
              plugins.entries.{pluginId}.config
            </p>
          </div>
          <button onClick={closeWithConfirm} className="text-gray-400 hover:text-gray-600">
            <X size={18} />
          </button>
        </div>

        <div className="flex-1 min-h-[320px] p-4">
          {isLoading ? (
            <p className="text-sm text-gray-400">Loading…</p>
          ) : (
            <MonacoConfigEditor value={value} onChange={handleChange} language="json" height="100%" />
          )}
        </div>

        {jsonError && (
          <p className="px-6 pb-2 text-xs text-red-600">Invalid JSON: {jsonError}</p>
        )}

        <div className="px-6 py-4 border-t border-gray-200 flex items-center justify-end gap-3">
          <button
            type="button"
            onClick={closeWithConfirm}
            className="px-4 py-2 text-sm font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50"
          >
            Close
          </button>
          <button
            type="button"
            onClick={handleSave}
            disabled={!dirty || !!jsonError || save.isPending}
            className="px-4 py-2 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {save.isPending ? "Saving…" : "Save & restart gateway"}
          </button>
        </div>
      </div>
    </div>
  );
}
