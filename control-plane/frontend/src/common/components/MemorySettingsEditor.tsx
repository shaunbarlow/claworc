import { useMemo, useState } from "react";
import type { IndexedFolder, MemoryQmdSettings } from "@common/types/instance";

export interface MemoryBackendOption {
  value: "" | "builtin" | "qmd";
  label: string;
}

interface Props {
  title: string;
  description: string;
  backend: "" | "builtin" | "qmd";
  backendOptions: MemoryBackendOption[];
  qmd: MemoryQmdSettings;
  /** Resolved values shown as placeholders when a field is unset (agent page). */
  effectiveQmd?: MemoryQmdSettings;
  /** Shared folders feeding the QMD index (agent page). */
  indexedFolders?: IndexedFolder[];
  /** Backend that applies when the "" (inherit) option is selected. */
  inheritBackend?: "builtin" | "qmd";
  /** Shown under the Save button, e.g. the gateway-restart warning. */
  footnote?: string;
  onSave: (backend: "" | "builtin" | "qmd", qmd: MemoryQmdSettings) => Promise<void>;
  isSaving: boolean;
}

const inputCls =
  "w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm focus:outline-none focus:ring-2 focus:ring-blue-500";

/** Card editing the OpenClaw memory backend selection plus the curated QMD
 * knobs and the advanced-JSON escape hatch. Self-contained save, following
 * the SimpleKVEditor/PortsEditor pattern. */
export default function MemorySettingsEditor({
  title,
  description,
  backend,
  backendOptions,
  qmd,
  effectiveQmd,
  indexedFolders,
  inheritBackend,
  footnote,
  onSave,
  isSaving,
}: Props) {
  const [draftBackend, setDraftBackend] = useState<"" | "builtin" | "qmd">(backend);
  const [searchMode, setSearchMode] = useState(qmd.search_mode ?? "");
  const [updateInterval, setUpdateInterval] = useState(qmd.update_interval ?? "");
  const [maxResults, setMaxResults] = useState(qmd.max_results != null ? String(qmd.max_results) : "");
  const [sessionsEnabled, setSessionsEnabled] = useState<"" | "true" | "false">(
    qmd.sessions_enabled == null ? "" : qmd.sessions_enabled ? "true" : "false",
  );
  const [includeDefaultMemory, setIncludeDefaultMemory] = useState<"" | "true" | "false">(
    qmd.include_default_memory == null ? "" : qmd.include_default_memory ? "true" : "false",
  );
  const [advanced, setAdvanced] = useState(
    qmd.advanced && Object.keys(qmd.advanced).length > 0 ? JSON.stringify(qmd.advanced, null, 2) : "",
  );
  const [advancedOpen, setAdvancedOpen] = useState(!!advanced);

  const advancedError = useMemo(() => {
    if (!advanced.trim()) return null;
    try {
      const parsed = JSON.parse(advanced);
      if (parsed === null || Array.isArray(parsed) || typeof parsed !== "object") {
        return "Must be a JSON object";
      }
      return null;
    } catch {
      return "Invalid JSON";
    }
  }, [advanced]);

  const intervalError =
    updateInterval && !/^\d+(ms|s|m|h)$/.test(updateInterval)
      ? 'Use a duration like "30s", "5m" or "1h"'
      : null;

  const buildQmd = (): MemoryQmdSettings => {
    const out: MemoryQmdSettings = {};
    if (searchMode) out.search_mode = searchMode as MemoryQmdSettings["search_mode"];
    if (updateInterval) out.update_interval = updateInterval;
    if (maxResults) out.max_results = Number(maxResults);
    if (sessionsEnabled) out.sessions_enabled = sessionsEnabled === "true";
    if (includeDefaultMemory) out.include_default_memory = includeDefaultMemory === "true";
    if (advanced.trim() && !advancedError) out.advanced = JSON.parse(advanced);
    return out;
  };

  const dirty = useMemo(() => {
    const current = JSON.stringify({ b: backend, q: qmd });
    try {
      return current !== JSON.stringify({ b: draftBackend, q: buildQmd() });
    } catch {
      return true;
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backend, qmd, draftBackend, searchMode, updateInterval, maxResults, sessionsEnabled, includeDefaultMemory, advanced, advancedError]);

  const canSave = dirty && !isSaving && !advancedError && !intervalError;
  const effectiveDraftBackend = draftBackend === "" ? (inheritBackend ?? "builtin") : draftBackend;
  const qmdVisible = effectiveDraftBackend === "qmd";

  const triState = (
    label: string,
    value: "" | "true" | "false",
    setValue: (v: "" | "true" | "false") => void,
    inheritLabel: string,
  ) => (
    <div>
      <label className="block text-xs text-gray-500 mb-1">{label}</label>
      <select value={value} onChange={(e) => setValue(e.target.value as "" | "true" | "false")} className={inputCls}>
        <option value="">{inheritLabel}</option>
        <option value="true">Enabled</option>
        <option value="false">Disabled</option>
      </select>
    </div>
  );

  return (
    <div className="bg-white rounded-lg border border-gray-200 p-6">
      <h3 className="text-sm font-medium text-gray-900 mb-1">{title}</h3>
      <p className="text-xs text-gray-500 mb-4">{description}</p>
      <div className="space-y-4">
        <div>
          <label className="block text-xs text-gray-500 mb-1">Backend</label>
          <select
            value={draftBackend}
            onChange={(e) => setDraftBackend(e.target.value as "" | "builtin" | "qmd")}
            className={inputCls}
          >
            {backendOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </div>

        {qmdVisible && (
          <>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Search Mode</label>
                <select value={searchMode} onChange={(e) => setSearchMode(e.target.value)} className={inputCls}>
                  <option value="">
                    {effectiveQmd?.search_mode ? `Inherit (${effectiveQmd.search_mode})` : "Default (search)"}
                  </option>
                  <option value="search">search — BM25, fastest</option>
                  <option value="vsearch">vsearch — vector similarity</option>
                  <option value="query">query — full rerank, slow on CPU</option>
                </select>
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">Reindex Interval</label>
                <input
                  type="text"
                  value={updateInterval}
                  onChange={(e) => setUpdateInterval(e.target.value)}
                  placeholder={effectiveQmd?.update_interval ?? "5m"}
                  className={inputCls}
                />
                {intervalError && <p className="mt-1 text-xs text-red-600">{intervalError}</p>}
              </div>
            </div>
            <div className="grid grid-cols-3 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Max Results</label>
                <input
                  type="number"
                  min={1}
                  max={50}
                  value={maxResults}
                  onChange={(e) => setMaxResults(e.target.value)}
                  placeholder={effectiveQmd?.max_results != null ? String(effectiveQmd.max_results) : "6"}
                  className={inputCls}
                />
              </div>
              {triState(
                "Index Sessions",
                sessionsEnabled,
                setSessionsEnabled,
                effectiveQmd?.sessions_enabled != null
                  ? `Inherit (${effectiveQmd.sessions_enabled ? "enabled" : "disabled"})`
                  : "Default (disabled)",
              )}
              {triState(
                "Workspace Memory",
                includeDefaultMemory,
                setIncludeDefaultMemory,
                effectiveQmd?.include_default_memory != null
                  ? `Inherit (${effectiveQmd.include_default_memory ? "enabled" : "disabled"})`
                  : "Default (enabled)",
              )}
            </div>

            {indexedFolders !== undefined && (
              <div>
                <label className="block text-xs text-gray-500 mb-1">Indexed Shared Folders</label>
                {indexedFolders.length === 0 ? (
                  <p className="text-xs text-gray-400">
                    None. Enable "Include in memory index" on a shared folder attached to this agent.
                  </p>
                ) : (
                  <ul className="text-xs text-gray-700 space-y-1">
                    {indexedFolders.map((f) => (
                      <li key={f.id} className="flex items-center gap-2">
                        <span className="font-medium">{f.name}</span>
                        <span className="text-gray-400 font-mono">
                          {f.mount_path}/{f.pattern}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}

            <div>
              <button
                type="button"
                onClick={() => setAdvancedOpen((v) => !v)}
                className="text-xs text-blue-600 hover:text-blue-800"
              >
                {advancedOpen ? "Hide advanced config" : "Advanced config (JSON)"}
              </button>
              {advancedOpen && (
                <div className="mt-2">
                  <textarea
                    value={advanced}
                    onChange={(e) => setAdvanced(e.target.value)}
                    rows={5}
                    spellCheck={false}
                    placeholder='{"limits": {"timeoutMs": 8000}}'
                    className={`${inputCls} font-mono`}
                  />
                  <p className="mt-1 text-xs text-gray-500">
                    Merged into OpenClaw's <code>memory.qmd</code> config last — any option Claworc doesn't model
                    (scope rules, timeouts, debounce) goes here.
                  </p>
                  {advancedError && <p className="mt-1 text-xs text-red-600">{advancedError}</p>}
                </div>
              )}
            </div>
          </>
        )}

        <div className="flex items-center justify-between pt-1">
          <span className="text-xs text-amber-600">{footnote ?? ""}</span>
          <button
            type="button"
            disabled={!canSave}
            onClick={() => onSave(draftBackend, buildQmd())}
            className="px-3 py-1.5 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {isSaving ? "Saving..." : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}
