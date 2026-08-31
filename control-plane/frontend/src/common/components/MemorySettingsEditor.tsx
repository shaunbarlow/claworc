import { useMemo, useState } from "react";
import type { IndexedFolder, MemorySettings } from "@common/types/instance";

/** Curated dropdown of embedding providers OpenClaw's builtin memory engine
 * ships with (see /reference/memory-config). Anything else (a custom
 * `models.providers.<id>` reference) still round-trips via the Advanced
 * JSON field or by typing it directly if a free-text override is added
 * later — this list only covers the common case. */
const PROVIDER_OPTIONS: { value: string; label: string }[] = [
  { value: "", label: "Auto (OpenAI if configured, else FTS-only)" },
  { value: "openai", label: "OpenAI" },
  { value: "gemini", label: "Gemini" },
  { value: "bedrock", label: "Bedrock" },
  { value: "deepinfra", label: "DeepInfra" },
  { value: "mistral", label: "Mistral" },
  { value: "voyage", label: "Voyage" },
  { value: "ollama", label: "Ollama (local/self-hosted)" },
  { value: "lmstudio", label: "LM Studio (local/self-hosted)" },
  { value: "local", label: "Local (managed llama.cpp)" },
  { value: "openai-compatible", label: "OpenAI-compatible endpoint" },
  { value: "none", label: "None (FTS keyword search only)" },
];

export interface Props {
  title: string;
  description: string;
  settings: MemorySettings;
  /** Resolved values shown as placeholders when a field is unset (agent page). */
  effectiveSettings?: MemorySettings;
  /** Shared folders feeding the memory index (agent page). */
  indexedFolders?: IndexedFolder[];
  /** Shown under the Save button, e.g. the gateway-restart warning. */
  footnote?: string;
  onSave: (settings: MemorySettings) => Promise<void>;
  isSaving: boolean;
}

const inputCls =
  "w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm focus:outline-none focus:ring-2 focus:ring-blue-500";

/** Card editing OpenClaw's builtin memory.search.* config (embedding
 * provider, query limits, citations, session-transcript indexing) plus the
 * advanced-JSON escape hatch. Self-contained save, following the
 * SimpleKVEditor/PortsEditor pattern. */
export default function MemorySettingsEditor({
  title,
  description,
  settings,
  effectiveSettings,
  indexedFolders,
  footnote,
  onSave,
  isSaving,
}: Props) {
  const [provider, setProvider] = useState(settings.provider ?? "");
  const [model, setModel] = useState(settings.model ?? "");
  const [fallback, setFallback] = useState(settings.fallback ?? "");
  const [maxResults, setMaxResults] = useState(settings.max_results != null ? String(settings.max_results) : "");
  const [minScore, setMinScore] = useState(settings.min_score != null ? String(settings.min_score) : "");
  const [citations, setCitations] = useState<"" | "auto" | "on" | "off">(settings.citations ?? "");
  const [rememberAcrossConversations, setRememberAcrossConversations] = useState<"" | "true" | "false">(
    settings.remember_across_conversations == null ? "" : settings.remember_across_conversations ? "true" : "false",
  );
  const [sessionsEnabled, setSessionsEnabled] = useState<"" | "true" | "false">(
    settings.sessions_enabled == null ? "" : settings.sessions_enabled ? "true" : "false",
  );
  const [advanced, setAdvanced] = useState(
    settings.advanced && Object.keys(settings.advanced).length > 0 ? JSON.stringify(settings.advanced, null, 2) : "",
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

  const maxResultsError =
    maxResults && (!/^\d+$/.test(maxResults) || Number(maxResults) < 1 || Number(maxResults) > 100)
      ? "Must be a whole number between 1 and 100"
      : null;
  const minScoreError =
    minScore && (Number.isNaN(Number(minScore)) || Number(minScore) < 0 || Number(minScore) > 1)
      ? "Must be a number between 0 and 1"
      : null;

  const buildSettings = (): MemorySettings => {
    const out: MemorySettings = {};
    if (provider) out.provider = provider;
    if (model.trim()) out.model = model.trim();
    if (fallback) out.fallback = fallback;
    if (maxResults && !maxResultsError) out.max_results = Number(maxResults);
    if (minScore && !minScoreError) out.min_score = Number(minScore);
    if (citations) out.citations = citations;
    if (rememberAcrossConversations) out.remember_across_conversations = rememberAcrossConversations === "true";
    if (sessionsEnabled) out.sessions_enabled = sessionsEnabled === "true";
    if (advanced.trim() && !advancedError) out.advanced = JSON.parse(advanced);
    return out;
  };

  const dirty = useMemo(() => {
    const current = JSON.stringify(settings);
    try {
      return current !== JSON.stringify(buildSettings());
    } catch {
      return true;
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    settings,
    provider,
    model,
    fallback,
    maxResults,
    minScore,
    citations,
    rememberAcrossConversations,
    sessionsEnabled,
    advanced,
    advancedError,
  ]);

  const canSave = dirty && !isSaving && !advancedError && !maxResultsError && !minScoreError;

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
        <div className="grid grid-cols-2 gap-4">
          <div>
            <label className="block text-xs text-gray-500 mb-1">Embedding Provider</label>
            <select value={provider} onChange={(e) => setProvider(e.target.value)} className={inputCls}>
              {PROVIDER_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {effectiveSettings?.provider && o.value === "" ? `Inherit (${effectiveSettings.provider})` : o.label}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className="block text-xs text-gray-500 mb-1">Model Override</label>
            <input
              type="text"
              value={model}
              onChange={(e) => setModel(e.target.value)}
              placeholder={effectiveSettings?.model ?? "Provider default"}
              className={inputCls}
            />
          </div>
        </div>

        <div className="grid grid-cols-3 gap-4">
          <div>
            <label className="block text-xs text-gray-500 mb-1">Fallback Provider</label>
            <select value={fallback} onChange={(e) => setFallback(e.target.value)} className={inputCls}>
              <option value="">{effectiveSettings?.fallback ? `Inherit (${effectiveSettings.fallback})` : "None"}</option>
              {PROVIDER_OPTIONS.filter((o) => o.value && o.value !== "none").map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className="block text-xs text-gray-500 mb-1">Max Results</label>
            <input
              type="number"
              min={1}
              max={100}
              value={maxResults}
              onChange={(e) => setMaxResults(e.target.value)}
              placeholder={effectiveSettings?.max_results != null ? String(effectiveSettings.max_results) : "6"}
              className={inputCls}
            />
            {maxResultsError && <p className="mt-1 text-xs text-red-600">{maxResultsError}</p>}
          </div>
          <div>
            <label className="block text-xs text-gray-500 mb-1">Min Score</label>
            <input
              type="text"
              value={minScore}
              onChange={(e) => setMinScore(e.target.value)}
              placeholder={effectiveSettings?.min_score != null ? String(effectiveSettings.min_score) : "0.0-1.0"}
              className={inputCls}
            />
            {minScoreError && <p className="mt-1 text-xs text-red-600">{minScoreError}</p>}
          </div>
        </div>

        <div className="grid grid-cols-3 gap-4">
          <div>
            <label className="block text-xs text-gray-500 mb-1">Citations</label>
            <select value={citations} onChange={(e) => setCitations(e.target.value as "" | "auto" | "on" | "off")} className={inputCls}>
              <option value="">{effectiveSettings?.citations ? `Inherit (${effectiveSettings.citations})` : "Default (auto)"}</option>
              <option value="auto">Auto — include when useful</option>
              <option value="on">Always on</option>
              <option value="off">Off</option>
            </select>
          </div>
          {triState(
            "Cross-Conversation Recall",
            rememberAcrossConversations,
            setRememberAcrossConversations,
            effectiveSettings?.remember_across_conversations != null
              ? `Inherit (${effectiveSettings.remember_across_conversations ? "enabled" : "disabled"})`
              : "Default",
          )}
          {triState(
            "Index Session Transcripts",
            sessionsEnabled,
            setSessionsEnabled,
            effectiveSettings?.sessions_enabled != null
              ? `Inherit (${effectiveSettings.sessions_enabled ? "enabled" : "disabled"})`
              : "Default (disabled)",
          )}
        </div>
        <p className="text-xs text-gray-500">
          Cross-Conversation Recall lets this agent recall context from its own other recognized private
          conversations (implies session indexing). Index Session Transcripts alone just makes past sessions
          searchable via <code>memory_search</code> without that broader recall.
        </p>

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
                      {f.mount_path}
                      {f.pattern ? `/${f.pattern}` : ""}
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
                placeholder='{"multimodal": {"enabled": true, "modalities": ["image"]}}'
                className={`${inputCls} font-mono`}
              />
              <p className="mt-1 text-xs text-gray-500">
                Merged into OpenClaw's <code>memory.search</code> config last — any option Claworc doesn't model
                (multimodal, remote endpoint/headers, store.vector, cache, input-type labels) goes here.
              </p>
              {advancedError && <p className="mt-1 text-xs text-red-600">{advancedError}</p>}
            </div>
          )}
        </div>

        <div className="flex items-center justify-between pt-1">
          <span className="text-xs text-amber-600">{footnote ?? ""}</span>
          <button
            type="button"
            disabled={!canSave}
            onClick={() => onSave(buildSettings())}
            className="px-3 py-1.5 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {isSaving ? "Saving..." : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}
