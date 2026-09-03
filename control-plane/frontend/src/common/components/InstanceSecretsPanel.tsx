import { useState } from "react";
import { Copy, Eye, EyeOff, KeyRound, Pencil, Plus, Trash2 } from "lucide-react";
import EditInput from "@common/components/EditInput";
import ConfirmDialog from "@common/components/ConfirmDialog";
import { revealInstanceSecret } from "@common/api/openbao";
import type { SecretEntry } from "@common/api/openbao";
import {
  useDeleteInstanceSecret,
  useInstanceSecrets,
  useWriteInstanceSecret,
} from "@common/hooks/useOpenbao";
import { errorToast, infoToast } from "@common/utils/toast";

interface Props {
  instanceId: number;
}

/** Identity of one field within one secret, used to key local reveal state. */
function fieldId(path: string, key: string) {
  return `${path}#${key}`;
}

/**
 * InstanceSecretsPanel browses and edits one agent's own OpenBao namespace
 * (secret/agents/<uuid>/**): every secret in it, and a form to set a single
 * field on any path, existing or not.
 *
 * Values arrive masked; the plaintext of one field is fetched only when the
 * admin explicitly reveals or copies it (see RevealInstanceSecret, which
 * logs each such read). Rendered only for admins with the managed OpenBao
 * deployment enabled — the parent gates that.
 */
export default function InstanceSecretsPanel({ instanceId }: Props) {
  const { data, isLoading, error } = useInstanceSecrets(instanceId, true);
  const writeMutation = useWriteInstanceSecret(instanceId);
  const deleteMutation = useDeleteInstanceSecret(instanceId);

  const [formOpen, setFormOpen] = useState(false);
  const [path, setPath] = useState("");
  const [key, setKey] = useState("value");
  const [value, setValue] = useState("");
  const [showValue, setShowValue] = useState(false);
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [pendingDelete, setPendingDelete] = useState<{ path: string; key?: string } | null>(null);

  const canSave = path.trim() !== "" && key.trim() !== "" && value !== "";

  const resetForm = () => {
    setFormOpen(false);
    setPath("");
    setKey("value");
    setValue("");
    setShowValue(false);
  };

  const openFormFor = (entryPath: string, entryKey: string) => {
    setPath(entryPath);
    setKey(entryKey);
    setValue("");
    setShowValue(false);
    setFormOpen(true);
  };

  /** Drops any revealed plaintext for a field whose value just changed. */
  const forgetRevealed = (entryPath: string, entryKey?: string) => {
    setRevealed((prev) => {
      const next = { ...prev };
      for (const id of Object.keys(next)) {
        if (id === fieldId(entryPath, entryKey ?? "") || (!entryKey && id.startsWith(`${entryPath}#`))) {
          delete next[id];
        }
      }
      return next;
    });
  };

  const handleSave = async () => {
    if (!canSave) return;
    const savedPath = path.trim();
    const savedKey = key.trim();
    await writeMutation.mutateAsync({ path: savedPath, key: savedKey, value });
    // The old plaintext is no longer what's stored, so stop showing it.
    forgetRevealed(savedPath, savedKey);
    resetForm();
  };

  const loadValue = async (entryPath: string, entryKey: string): Promise<string | null> => {
    try {
      return await revealInstanceSecret(instanceId, entryPath, entryKey);
    } catch (err) {
      errorToast("Failed to read secret", err);
      return null;
    }
  };

  const toggleReveal = async (entryPath: string, entryKey: string) => {
    const id = fieldId(entryPath, entryKey);
    if (revealed[id] !== undefined) {
      setRevealed((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      return;
    }
    const plaintext = await loadValue(entryPath, entryKey);
    if (plaintext !== null) setRevealed((prev) => ({ ...prev, [id]: plaintext }));
  };

  const copyValue = async (entryPath: string, entryKey: string) => {
    const id = fieldId(entryPath, entryKey);
    const plaintext = revealed[id] ?? (await loadValue(entryPath, entryKey));
    if (plaintext === null) return;
    try {
      await navigator.clipboard.writeText(plaintext);
      infoToast("Value copied to clipboard");
    } catch (err) {
      errorToast("Failed to copy value", err);
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    await deleteMutation.mutateAsync(pendingDelete);
    forgetRevealed(pendingDelete.path, pendingDelete.key);
    setPendingDelete(null);
  };

  const entries: SecretEntry[] = data?.entries ?? [];

  return (
    <div className="bg-white rounded-lg border border-gray-200 p-6">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-medium text-gray-900 flex items-center gap-1.5">
          <KeyRound size={14} />
          Secrets (OpenBao)
        </h3>
        {!formOpen && data?.ready && (
          <button
            type="button"
            onClick={() => setFormOpen(true)}
            className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50"
          >
            <Plus size={12} />
            Set value
          </button>
        )}
      </div>
      <p className="text-xs text-gray-500 mb-4">
        This agent's own secret namespace, which it can read and write itself with{" "}
        <code className="font-mono">bao kv</code>. Paths below are relative to{" "}
        <span className="font-mono">{data?.base_path ?? "secret/agents/…"}</span>.
      </p>

      {formOpen && (
        <div className="bg-gray-50 border border-gray-200 rounded-md p-4 mb-4">
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-xs text-gray-500 mb-1">Path *</label>
              <EditInput
                type="text"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                onSave={handleSave}
                onCancel={resetForm}
                placeholder="github/token"
                className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
              />
              <p className="text-xs text-gray-400 mt-1">
                Created if it doesn't exist. Slashes make sub-paths.
              </p>
            </div>
            <div>
              <label className="block text-xs text-gray-500 mb-1">Key *</label>
              <EditInput
                type="text"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                onSave={handleSave}
                onCancel={resetForm}
                placeholder="value"
                className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
              />
              <p className="text-xs text-gray-400 mt-1">
                Other keys on the same path are left untouched.
              </p>
            </div>
          </div>
          <div className="mt-4">
            <label className="block text-xs text-gray-500 mb-1">Value *</label>
            <div className="relative">
              <EditInput
                type={showValue ? "text" : "password"}
                value={value}
                onChange={(e) => setValue(e.target.value)}
                onSave={handleSave}
                onCancel={resetForm}
                autoComplete="off"
                className="w-full px-3 py-1.5 pr-10 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
              />
              <button
                type="button"
                onClick={() => setShowValue(!showValue)}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600"
              >
                {showValue ? <EyeOff size={14} /> : <Eye size={14} />}
              </button>
            </div>
          </div>
          <div className="flex justify-end gap-3 mt-4">
            <button
              type="button"
              onClick={resetForm}
              disabled={writeMutation.isPending}
              className="px-4 py-2 text-sm font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={handleSave}
              disabled={!canSave || writeMutation.isPending}
              className="px-4 py-2 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {writeMutation.isPending ? "Saving..." : "Save"}
            </button>
          </div>
        </div>
      )}

      {error ? (
        <p className="text-sm text-red-600">Failed to load secrets.</p>
      ) : data && !data.enabled ? (
        <p className="text-sm text-gray-400 italic">
          The managed OpenBao deployment is disabled in Settings.
        </p>
      ) : data && !data.ready ? (
        <p className="text-sm text-gray-400 italic">OpenBao is still starting up.</p>
      ) : isLoading ? (
        <p className="text-sm text-gray-400 italic">Loading secrets...</p>
      ) : entries.length === 0 ? (
        <p className="text-sm text-gray-400 italic">
          No secrets yet. Anything this agent writes shows up here.
        </p>
      ) : (
        <div className="divide-y divide-gray-100">
          {entries.map((entry) => (
            <div key={entry.path} className="py-3 first:pt-0">
              <div className="flex items-center justify-between gap-4">
                <span className="text-sm font-mono text-gray-900 break-all">{entry.path}</span>
                <div className="flex items-center gap-3 shrink-0">
                  <span className="text-xs text-gray-400">v{entry.version}</span>
                  <button
                    type="button"
                    onClick={() => setPendingDelete({ path: entry.path })}
                    title="Delete this secret and every version of it"
                    className="text-gray-400 hover:text-red-600"
                  >
                    <Trash2 size={14} />
                  </button>
                </div>
              </div>
              <div className="mt-1.5 space-y-1">
                {entry.fields.map((field) => {
                  const id = fieldId(entry.path, field.key);
                  const plaintext = revealed[id];
                  return (
                    <div key={field.key} className="flex items-center gap-3">
                      <span className="text-xs font-mono text-gray-500 w-40 shrink-0 truncate">
                        {field.key}
                      </span>
                      <span className="text-xs font-mono text-gray-900 break-all grow">
                        {plaintext ?? field.masked}
                      </span>
                      <div className="flex items-center gap-2 shrink-0">
                        <button
                          type="button"
                          onClick={() => toggleReveal(entry.path, field.key)}
                          title={plaintext ? "Hide value" : "Reveal value"}
                          className="text-gray-400 hover:text-gray-600"
                        >
                          {plaintext ? <EyeOff size={14} /> : <Eye size={14} />}
                        </button>
                        <button
                          type="button"
                          onClick={() => copyValue(entry.path, field.key)}
                          title="Copy value"
                          className="text-gray-400 hover:text-gray-600"
                        >
                          <Copy size={14} />
                        </button>
                        <button
                          type="button"
                          onClick={() => openFormFor(entry.path, field.key)}
                          title="Set a new value"
                          className="text-gray-400 hover:text-blue-600"
                        >
                          <Pencil size={14} />
                        </button>
                        <button
                          type="button"
                          onClick={() => setPendingDelete({ path: entry.path, key: field.key })}
                          title="Delete this key"
                          className="text-gray-400 hover:text-red-600"
                        >
                          <Trash2 size={14} />
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          ))}
        </div>
      )}

      {data?.truncated && (
        <p className="text-xs text-amber-600 mt-3">
          Only the first 200 secrets are listed. Use the <code className="font-mono">bao</code> CLI
          to browse the rest.
        </p>
      )}

      {pendingDelete && (
        <ConfirmDialog
          title={pendingDelete.key ? "Delete this key?" : "Delete this secret?"}
          message={
            pendingDelete.key
              ? `"${pendingDelete.key}" will be removed from ${pendingDelete.path}. If it is the last key, the secret itself is deleted. The agent loses access to it immediately.`
              : `${pendingDelete.path} and every version of it will be permanently deleted. The agent loses access to it immediately.`
          }
          confirmLabel="Delete"
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />
      )}
    </div>
  );
}
