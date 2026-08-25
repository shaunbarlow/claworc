import { useMemo, useState } from "react";
import { Plus, Trash2 } from "lucide-react";
import type { LosslessClawFallbackProvider, LosslessClawSettings } from "@common/types/instance";

export interface ContextEngineOption {
  value: "" | "legacy" | "lossless-claw";
  label: string;
}

interface Props {
  title: string;
  description: string;
  engine: "" | "legacy" | "lossless-claw";
  engineOptions: ContextEngineOption[];
  losslessClaw: LosslessClawSettings;
  /** Resolved values shown as placeholders when a field is unset (agent page). */
  effectiveLosslessClaw?: LosslessClawSettings;
  /** Engine that applies when the "" (inherit) option is selected. */
  inheritEngine?: "legacy" | "lossless-claw";
  /** Shown under the Save button, e.g. a restart warning. */
  footnote?: string;
  onSave: (engine: "" | "legacy" | "lossless-claw", losslessClaw: LosslessClawSettings) => Promise<void>;
  isSaving: boolean;
}

const inputCls =
  "w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm focus:outline-none focus:ring-2 focus:ring-blue-500";

/** Card editing the OpenClaw context-engine slot selection (plugins.slots.contextEngine)
 * plus the curated lossless-claw knobs and the advanced-JSON escape hatch.
 * Self-contained save, following the MemorySettingsEditor pattern. */
export default function ContextEngineSettingsEditor({
  title,
  description,
  engine,
  engineOptions,
  losslessClaw,
  effectiveLosslessClaw,
  inheritEngine,
  footnote,
  onSave,
  isSaving,
}: Props) {
  const [draftEngine, setDraftEngine] = useState<"" | "legacy" | "lossless-claw">(engine);
  const [contextThreshold, setContextThreshold] = useState(
    losslessClaw.context_threshold != null ? String(losslessClaw.context_threshold) : "",
  );
  const [freshTailCount, setFreshTailCount] = useState(
    losslessClaw.fresh_tail_count != null ? String(losslessClaw.fresh_tail_count) : "",
  );
  const [leafChunkTokens, setLeafChunkTokens] = useState(
    losslessClaw.leaf_chunk_tokens != null ? String(losslessClaw.leaf_chunk_tokens) : "",
  );
  const [sweepMaxDepth, setSweepMaxDepth] = useState(
    losslessClaw.sweep_max_depth != null ? String(losslessClaw.sweep_max_depth) : "",
  );
  const [hostFallbackMode, setHostFallbackMode] = useState(losslessClaw.host_fallback_mode ?? "");
  const [promptAwareEviction, setPromptAwareEviction] = useState<"" | "true" | "false">(
    losslessClaw.prompt_aware_eviction == null ? "" : losslessClaw.prompt_aware_eviction ? "true" : "false",
  );
  const [stubLargeToolPayloads, setStubLargeToolPayloads] = useState<"" | "true" | "false">(
    losslessClaw.stub_large_tool_payloads == null ? "" : losslessClaw.stub_large_tool_payloads ? "true" : "false",
  );
  const [customInstructions, setCustomInstructions] = useState(losslessClaw.custom_instructions ?? "");
  const [summaryModel, setSummaryModel] = useState(losslessClaw.summary_model ?? "");
  const [summaryProvider, setSummaryProvider] = useState(losslessClaw.summary_provider ?? "");
  const [fallbackProviders, setFallbackProviders] = useState<LosslessClawFallbackProvider[]>(
    losslessClaw.fallback_providers ?? [],
  );
  const [advanced, setAdvanced] = useState(
    losslessClaw.advanced && Object.keys(losslessClaw.advanced).length > 0
      ? JSON.stringify(losslessClaw.advanced, null, 2)
      : "",
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

  const thresholdNum = contextThreshold ? Number(contextThreshold) : null;
  const thresholdError =
    thresholdNum != null && (Number.isNaN(thresholdNum) || thresholdNum < 0 || thresholdNum > 1)
      ? "Must be between 0 and 1"
      : null;
  const sweepDepthNum = sweepMaxDepth ? Number(sweepMaxDepth) : null;
  const sweepDepthError =
    sweepDepthNum != null && (Number.isNaN(sweepDepthNum) || !Number.isInteger(sweepDepthNum) || sweepDepthNum < -1)
      ? "Must be -1 or a non-negative integer"
      : null;

  const buildLosslessClaw = (): LosslessClawSettings => {
    const out: LosslessClawSettings = {};
    if (contextThreshold) out.context_threshold = Number(contextThreshold);
    if (freshTailCount) out.fresh_tail_count = Number(freshTailCount);
    if (leafChunkTokens) out.leaf_chunk_tokens = Number(leafChunkTokens);
    if (sweepMaxDepth) out.sweep_max_depth = Number(sweepMaxDepth);
    if (hostFallbackMode) out.host_fallback_mode = hostFallbackMode as LosslessClawSettings["host_fallback_mode"];
    if (promptAwareEviction) out.prompt_aware_eviction = promptAwareEviction === "true";
    if (stubLargeToolPayloads) out.stub_large_tool_payloads = stubLargeToolPayloads === "true";
    if (customInstructions) out.custom_instructions = customInstructions;
    if (summaryModel) out.summary_model = summaryModel;
    if (summaryProvider) out.summary_provider = summaryProvider;
    const cleanFallbacks = fallbackProviders.filter((fp) => fp.provider.trim() && fp.model.trim());
    if (cleanFallbacks.length > 0) out.fallback_providers = cleanFallbacks;
    if (advanced.trim() && !advancedError) out.advanced = JSON.parse(advanced);
    return out;
  };

  const dirty = useMemo(() => {
    const current = JSON.stringify({ e: engine, l: losslessClaw });
    try {
      return current !== JSON.stringify({ e: draftEngine, l: buildLosslessClaw() });
    } catch {
      return true;
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    engine,
    losslessClaw,
    draftEngine,
    contextThreshold,
    freshTailCount,
    leafChunkTokens,
    sweepMaxDepth,
    hostFallbackMode,
    promptAwareEviction,
    stubLargeToolPayloads,
    customInstructions,
    summaryModel,
    summaryProvider,
    fallbackProviders,
    advanced,
    advancedError,
  ]);

  const canSave = dirty && !isSaving && !advancedError && !thresholdError && !sweepDepthError;
  const effectiveDraftEngine = draftEngine === "" ? (inheritEngine ?? "legacy") : draftEngine;
  const losslessVisible = effectiveDraftEngine === "lossless-claw";

  const triState = (
    label: string,
    value: "" | "true" | "false",
    setValue: (v: "" | "true" | "false") => void,
    inheritLabel: string,
    help?: string,
  ) => (
    <div>
      <label className="block text-xs text-gray-500 mb-1">{label}</label>
      <select value={value} onChange={(e) => setValue(e.target.value as "" | "true" | "false")} className={inputCls}>
        <option value="">{inheritLabel}</option>
        <option value="true">Enabled</option>
        <option value="false">Disabled</option>
      </select>
      {help && <p className="mt-1 text-xs text-gray-500">{help}</p>}
    </div>
  );

  return (
    <div className="bg-white rounded-lg border border-gray-200 p-6">
      <h3 className="text-sm font-medium text-gray-900 mb-1">{title}</h3>
      <p className="text-xs text-gray-500 mb-4">{description}</p>
      <div className="space-y-4">
        <div>
          <label className="block text-xs text-gray-500 mb-1">Context Engine</label>
          <select
            value={draftEngine}
            onChange={(e) => setDraftEngine(e.target.value as "" | "legacy" | "lossless-claw")}
            className={inputCls}
          >
            {engineOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <p className="mt-1 text-xs text-gray-500">
            Legacy is OpenClaw's built-in context assembly and single-summary compaction. Lossless Context
            Management (lossless-claw) is a plugin engine with DAG-based summarization and lossless recall
            tools.
          </p>
        </div>

        {losslessVisible && (
          <>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Context Threshold</label>
                <input
                  type="number"
                  min={0}
                  max={1}
                  step={0.05}
                  value={contextThreshold}
                  onChange={(e) => setContextThreshold(e.target.value)}
                  placeholder={
                    effectiveLosslessClaw?.context_threshold != null
                      ? String(effectiveLosslessClaw.context_threshold)
                      : "0.75"
                  }
                  className={inputCls}
                />
                {thresholdError && <p className="mt-1 text-xs text-red-600">{thresholdError}</p>}
                <p className="mt-1 text-xs text-gray-500">Fraction of context window (0-1) that triggers compaction.</p>
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">Fresh Tail Count</label>
                <input
                  type="number"
                  min={1}
                  value={freshTailCount}
                  onChange={(e) => setFreshTailCount(e.target.value)}
                  placeholder={
                    effectiveLosslessClaw?.fresh_tail_count != null
                      ? String(effectiveLosslessClaw.fresh_tail_count)
                      : "plugin default"
                  }
                  className={inputCls}
                />
                <p className="mt-1 text-xs text-gray-500">Recent messages always protected from compaction.</p>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Leaf Chunk Tokens</label>
                <input
                  type="number"
                  min={1}
                  value={leafChunkTokens}
                  onChange={(e) => setLeafChunkTokens(e.target.value)}
                  placeholder={
                    effectiveLosslessClaw?.leaf_chunk_tokens != null
                      ? String(effectiveLosslessClaw.leaf_chunk_tokens)
                      : "20000"
                  }
                  className={inputCls}
                />
                <p className="mt-1 text-xs text-gray-500">Max source tokens per compaction chunk before summarization.</p>
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">Sweep Max Depth</label>
                <input
                  type="number"
                  min={-1}
                  value={sweepMaxDepth}
                  onChange={(e) => setSweepMaxDepth(e.target.value)}
                  placeholder={
                    effectiveLosslessClaw?.sweep_max_depth != null
                      ? String(effectiveLosslessClaw.sweep_max_depth)
                      : "0 = leaf only, -1 = unlimited"
                  }
                  className={inputCls}
                />
                {sweepDepthError && <p className="mt-1 text-xs text-red-600">{sweepDepthError}</p>}
              </div>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Host Fallback Mode</label>
                <select
                  value={hostFallbackMode}
                  onChange={(e) => setHostFallbackMode(e.target.value)}
                  className={inputCls}
                >
                  <option value="">
                    {effectiveLosslessClaw?.host_fallback_mode
                      ? `Inherit (${effectiveLosslessClaw.host_fallback_mode})`
                      : "Default (error)"}
                  </option>
                  <option value="error">error — fail closed on CLI hosts</option>
                  <option value="capture-only">capture-only — allow capture/recall on generic CLI hosts</option>
                </select>
              </div>
              {triState(
                "Prompt-Aware Eviction",
                promptAwareEviction,
                setPromptAwareEviction,
                effectiveLosslessClaw?.prompt_aware_eviction != null
                  ? `Inherit (${effectiveLosslessClaw.prompt_aware_eviction ? "enabled" : "disabled"})`
                  : "Default (disabled)",
                "Keeps older context by prompt relevance under tight budgets; can reduce prompt-cache hit rates.",
              )}
            </div>
            <div className="grid grid-cols-1 gap-4">
              {triState(
                "Stub Large Tool Payloads",
                stubLargeToolPayloads,
                setStubLargeToolPayloads,
                effectiveLosslessClaw?.stub_large_tool_payloads != null
                  ? `Inherit (${effectiveLosslessClaw.stub_large_tool_payloads ? "enabled" : "disabled"})`
                  : "Default (disabled)",
                "Requires running the plugin's lcm-blob-migrate script first to populate large-content storage.",
              )}
            </div>
            <div>
              <label className="block text-xs text-gray-500 mb-1">Custom Instructions</label>
              <textarea
                value={customInstructions}
                onChange={(e) => setCustomInstructions(e.target.value)}
                rows={2}
                placeholder={effectiveLosslessClaw?.custom_instructions || "Optional guidance appended to compaction/recall behavior"}
                className={inputCls}
              />
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div>
                <label className="block text-xs text-gray-500 mb-1">Summary Provider</label>
                <input
                  type="text"
                  value={summaryProvider}
                  onChange={(e) => setSummaryProvider(e.target.value)}
                  placeholder={effectiveLosslessClaw?.summary_provider || "e.g. anthropic"}
                  className={inputCls}
                />
              </div>
              <div>
                <label className="block text-xs text-gray-500 mb-1">Summary Model</label>
                <input
                  type="text"
                  value={summaryModel}
                  onChange={(e) => setSummaryModel(e.target.value)}
                  placeholder={effectiveLosslessClaw?.summary_model || "e.g. claude-haiku"}
                  className={inputCls}
                />
              </div>
            </div>
            {(summaryModel || summaryProvider) && (
              <p className="text-xs text-amber-600 -mt-2">
                Setting a summary model/provider also grants the plugin a trust policy
                (<code>plugins.entries.lossless-claw.llm.allowModelOverride</code>) so the override actually takes
                effect.
              </p>
            )}

            <div>
              <div className="flex items-center justify-between mb-1">
                <label className="block text-xs text-gray-500">Fallback Providers</label>
                <button
                  type="button"
                  onClick={() => setFallbackProviders((prev) => [...prev, { provider: "", model: "" }])}
                  className="inline-flex items-center gap-1 text-xs text-blue-600 hover:text-blue-800"
                >
                  <Plus size={12} /> Add
                </button>
              </div>
              {fallbackProviders.length === 0 ? (
                <p className="text-xs text-gray-400">None. Tried in order if the primary summary call fails.</p>
              ) : (
                <div className="space-y-2">
                  {fallbackProviders.map((fp, i) => (
                    <div key={i} className="flex gap-2 items-center">
                      <input
                        type="text"
                        value={fp.provider}
                        onChange={(e) =>
                          setFallbackProviders((prev) =>
                            prev.map((p, idx) => (idx === i ? { ...p, provider: e.target.value } : p)),
                          )
                        }
                        placeholder="provider"
                        className={`${inputCls} flex-1`}
                      />
                      <input
                        type="text"
                        value={fp.model}
                        onChange={(e) =>
                          setFallbackProviders((prev) =>
                            prev.map((p, idx) => (idx === i ? { ...p, model: e.target.value } : p)),
                          )
                        }
                        placeholder="model"
                        className={`${inputCls} flex-1`}
                      />
                      <button
                        type="button"
                        onClick={() => setFallbackProviders((prev) => prev.filter((_, idx) => idx !== i))}
                        className="p-1.5 text-gray-400 hover:text-red-600 rounded"
                        title="Remove"
                      >
                        <Trash2 size={14} />
                      </button>
                    </div>
                  ))}
                </div>
              )}
            </div>

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
                    rows={6}
                    spellCheck={false}
                    placeholder='{"cacheAwareCompaction": {"enabled": true}, "maxSweepIterations": 3}'
                    className={`${inputCls} font-mono`}
                  />
                  <p className="mt-1 text-xs text-gray-500">
                    Merged into <code>plugins.entries.lossless-claw.config</code> last, using OpenClaw's own
                    camelCase key names — any of the plugin's ~50 settings Claworc doesn't model above
                    (contextThresholdOverrides, cacheAwareCompaction, dynamicLeafChunkTokens,
                    autoRotateSessionFiles, independentLogFile, timeouts, ...) goes here.
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
            onClick={() => onSave(draftEngine, buildLosslessClaw())}
            className="px-3 py-1.5 text-sm font-medium text-white bg-blue-600 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {isSaving ? "Saving..." : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}
