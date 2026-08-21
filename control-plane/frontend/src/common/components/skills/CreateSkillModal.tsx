import { useState } from "react";
import { Plus, X } from "lucide-react";
import { useCreateSkill } from "@common/hooks/useSkills";

interface Props {
  onClose: () => void;
  onCreated: (slug: string) => void;
}

const DEFAULT_BODY = `# Skill Name

Describe when and how the agent should use this skill.
`;

/**
 * Creates a new skill purely from form fields -- no zip file needed. Renders
 * a SKILL.md server-side from the slug/description/required-env-vars/body
 * fields and saves it into the library, exactly like a single-file zip
 * upload would. The in-browser file editor opens immediately afterwards so
 * the admin can keep refining the body or add more files.
 */
export default function CreateSkillModal({ onClose, onCreated }: Props) {
  const [slug, setSlug] = useState("");
  const [description, setDescription] = useState("");
  const [envVars, setEnvVars] = useState<string[]>([]);
  const [body, setBody] = useState(DEFAULT_BODY);
  const [error, setError] = useState<string | null>(null);
  const { mutate: create, isPending } = useCreateSkill();

  const slugValid = /^[a-z0-9]+(-[a-z0-9]+)*$/.test(slug);
  const canSubmit = slugValid && description.trim() !== "" && !isPending;

  const handleSubmit = () => {
    if (!canSubmit) return;
    setError(null);
    create(
      {
        slug,
        description: description.trim(),
        required_env_vars: envVars.map((v) => v.trim()).filter((v) => v !== ""),
        body,
      },
      {
        onSuccess: (skill) => {
          onCreated(skill.slug);
          onClose();
        },
        onError: (err) => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const detail: string | undefined = (err as any)?.response?.data?.detail;
          setError(detail ?? "Failed to create skill");
        },
      },
    );
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") onClose();
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
      onKeyDown={handleKeyDown}
      tabIndex={-1}
    >
      <div className="bg-white rounded-xl shadow-xl w-full max-w-2xl mx-4 flex flex-col max-h-[85vh]">
        <div className="flex items-center justify-between px-6 py-4 border-b border-gray-200">
          <h2 className="text-base font-semibold text-gray-900">Create Skill</h2>
          <button onClick={onClose} className="text-gray-400 hover:text-gray-600">
            <X size={18} />
          </button>
        </div>

        <div className="px-6 py-6 flex flex-col gap-4 overflow-y-auto">
          {error && (
            <div className="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700">
              {error}
            </div>
          )}

          <div>
            <label className="block text-xs font-medium text-gray-700 mb-1">Slug</label>
            <input
              type="text"
              value={slug}
              onChange={(e) => setSlug(e.target.value.trim())}
              placeholder="my-skill"
              autoFocus
              className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
            />
            <p className="text-[11px] text-gray-500 mt-0.5">
              Lowercase letters, digits, and hyphens only (e.g. my-skill). This becomes the
              SKILL.md <span className="font-medium">name</span> and cannot be changed later.
            </p>
          </div>

          <div>
            <label className="block text-xs font-medium text-gray-700 mb-1">Description</label>
            <input
              type="text"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="What this skill does and when the agent should use it"
              className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
            />
          </div>

          <div>
            <div className="flex items-center justify-between mb-1">
              <label className="block text-xs font-medium text-gray-700">
                Required env vars
              </label>
              <button
                type="button"
                onClick={() => setEnvVars([...envVars, ""])}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-700 bg-white border border-gray-300 rounded-md hover:bg-gray-50"
              >
                <Plus size={12} />
                Add
              </button>
            </div>
            <p className="text-[11px] text-gray-500 mb-2">
              Env var names this skill needs (e.g. API_KEY). Deploying warns instead of failing
              when one is missing on the target agent.
            </p>
            {envVars.length > 0 && (
              <div className="space-y-1.5">
                {envVars.map((v, idx) => (
                  <div key={idx} className="flex items-center gap-2">
                    <input
                      type="text"
                      value={v}
                      onChange={(e) =>
                        setEnvVars(envVars.map((ev, i) => (i === idx ? e.target.value : ev)))
                      }
                      placeholder="API_KEY"
                      className="flex-1 px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
                    />
                    <button
                      type="button"
                      title="Remove"
                      onClick={() => setEnvVars(envVars.filter((_, i) => i !== idx))}
                      className="p-1.5 text-gray-500 hover:text-red-600"
                    >
                      <X size={14} />
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>

          <div>
            <label className="block text-xs font-medium text-gray-700 mb-1">SKILL.md body</label>
            <textarea
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={10}
              className="w-full px-3 py-1.5 border border-gray-300 rounded-md text-sm font-mono focus:outline-none focus:ring-2 focus:ring-blue-500"
            />
            <p className="text-[11px] text-gray-500 mt-0.5">
              The markdown instructions the agent reads. You can keep refining this in the file
              editor after creating the skill.
            </p>
          </div>
        </div>

        <div className="px-6 pb-5 pt-3 border-t border-gray-100 flex items-center justify-end gap-3">
          <button
            onClick={onClose}
            className="px-4 py-2 text-sm font-medium text-gray-700 hover:text-gray-900 transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={handleSubmit}
            disabled={!canSubmit}
            className="px-4 py-2 text-sm font-medium text-white bg-blue-600 rounded-lg hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            {isPending ? "Creating…" : "Create & edit"}
          </button>
        </div>
      </div>
    </div>
  );
}
