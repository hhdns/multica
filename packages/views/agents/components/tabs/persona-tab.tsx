"use client";

import { useCallback, useState } from "react";
import { Loader2, Save, Sparkles, ThumbsUp, ThumbsDown, Plus, X, Wand2, Brain, ChevronDown, ChevronRight, Cpu, Activity, UserRound, Download, Upload, Pencil, Trash2, Check, RefreshCw } from "lucide-react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import type { Agent, AgentPersona, AgentMemory, PersonaLLMCall, UpdateAgentPersonaRequest } from "@multica/core/types";
import { api } from "@multica/core/api";
import { useConfigStore } from "@multica/core/config";
import { useWSEvent } from "@multica/core/realtime";
import { Button } from "@multica/ui/components/ui/button";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { Badge } from "@multica/ui/components/ui/badge";
import { Input } from "@multica/ui/components/ui/input";

interface PersonaTabProps {
  agent: Agent;
  canEdit: boolean;
}

interface TraitConfig {
  key: keyof Pick<
    AgentPersona,
    | "trait_thoroughness"
    | "trait_verbosity"
    | "trait_risk_appetite"
    | "trait_curiosity"
    | "trait_confidence"
  >;
  label: string;
  lowLabel: string;
  highLabel: string;
}

const TRAITS: TraitConfig[] = [
  { key: "trait_thoroughness", label: "Thoroughness", lowLabel: "Quick & lean", highLabel: "Deep & careful" },
  { key: "trait_verbosity", label: "Verbosity", lowLabel: "Terse", highLabel: "Verbose" },
  { key: "trait_risk_appetite", label: "Risk Appetite", lowLabel: "Conservative", highLabel: "Aggressive" },
  { key: "trait_curiosity", label: "Curiosity", lowLabel: "Focused", highLabel: "Exploratory" },
  { key: "trait_confidence", label: "Confidence", lowLabel: "Humble", highLabel: "Bold" },
];

const MOOD_LABELS: Record<string, string> = {
  calm: "Calm",
  energized: "Energized",
  cautious: "Cautious",
  playful: "Playful",
};

const MOOD_COLORS: Record<string, string> = {
  calm: "bg-blue-100 text-blue-700",
  energized: "bg-amber-100 text-amber-700",
  cautious: "bg-orange-100 text-orange-700",
  playful: "bg-purple-100 text-purple-700",
};

const SIGNAL_ICONS: Record<string, React.ReactNode> = {
  praise: <ThumbsUp className="h-3 w-3 text-emerald-500" />,
  criticism: <ThumbsDown className="h-3 w-3 text-rose-500" />,
  task_success: <ThumbsUp className="h-3 w-3 text-emerald-400" />,
  task_failure: <ThumbsDown className="h-3 w-3 text-rose-400" />,
  rework_requested: <ThumbsDown className="h-3 w-3 text-orange-400" />,
};

export function PersonaTab({ agent, canEdit }: PersonaTabProps) {
  const qc = useQueryClient();
  const queryKey = ["agent-persona", agent.id];

  const { data: persona, isLoading } = useQuery({
    queryKey,
    queryFn: () => api.getAgentPersona(agent.id),
  });

  // Invalidate persona when a task for this agent completes or fails —
  // the server updates mood asynchronously after those events.
  const onTaskDone = useCallback(
    (p: unknown) => {
      if ((p as { agent_id?: string }).agent_id === agent.id) {
        qc.invalidateQueries({ queryKey });
      }
    },
    [agent.id, qc, queryKey],
  );
  useWSEvent("task:completed", onTaskDone);
  useWSEvent("task:failed", onTaskDone);

  const mutation = useMutation({
    mutationFn: (data: UpdateAgentPersonaRequest) =>
      api.updateAgentPersona(agent.id, data),
    onSuccess: (updated) => {
      qc.setQueryData(queryKey, updated);
    },
  });

  const personaSynthesisBackend = useConfigStore((s) => s.personaSynthesisBackend);

  const [importing, setImporting] = useState(false);
  const [importResult, setImportResult] = useState<{ imported: number; skipped: number } | null>(null);
  const [importError, setImportError] = useState<string | null>(null);
  // pendingImport holds a parsed bundle + unmapped emails waiting for user to assign mappings.
  const [pendingImport, setPendingImport] = useState<{
    bundle: object;
    unmappedEmails: string[];
    mappings: Record<string, string>;
  } | null>(null);

  const { data: members } = useQuery({
    queryKey: ["workspace-members", agent.workspace_id],
    queryFn: () => api.listMembers(agent.workspace_id),
    enabled: !!pendingImport,
  });

  const handleExport = async () => {
    const blob = await api.exportAgentPersona(agent.id);
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `${agent.name.replace(/\s+/g, "-")}-persona.json`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const handleImport = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    e.target.value = "";
    setImportResult(null);
    setImportError(null);
    const text = await file.text();
    let bundle: { memories?: { category?: string; source_user_email?: string }[] };
    try { bundle = JSON.parse(text); } catch { setImportError("Invalid JSON file."); return; }

    const unmappedEmails = [
      ...new Set(
        (bundle.memories ?? [])
          .filter((m) => m.category === "user_preference" && m.source_user_email)
          .map((m) => m.source_user_email as string),
      ),
    ];

    if (unmappedEmails.length > 0) {
      setPendingImport({ bundle, unmappedEmails, mappings: {} });
      return;
    }
    await doImport(bundle, {});
  };

  const doImport = async (bundle: object, userMappings: Record<string, string>) => {
    setImporting(true);
    setImportError(null);
    try {
      const result = await api.importAgentPersona(agent.id, bundle, userMappings);
      setImportResult({ imported: result.memories_imported, skipped: result.memories_skipped });
      qc.invalidateQueries({ queryKey: ["agent-memories", agent.id] });
      qc.invalidateQueries({ queryKey });
      qc.invalidateQueries({ queryKey: ["agent-detail-probe", agent.workspace_id, agent.id] });
    } catch (err) {
      setImportError(err instanceof Error ? err.message : "Import failed.");
    } finally {
      setImporting(false);
      setPendingImport(null);
    }
  };

  const [synthesisError, setSynthesisError] = useState<string | null>(null);
  const synthesize = useMutation({
    mutationFn: () => api.synthesizeAgentPersona(agent.id),
    onSuccess: (updated) => {
      qc.setQueryData(queryKey, updated);
      setSynthesisError(null);
    },
    onError: (err: Error) => {
      setSynthesisError(err.message);
    },
  });

  if (isLoading || !persona) {
    return (
      <div className="flex h-full items-center justify-center">
        <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col gap-6 overflow-y-auto py-1 pr-1">
      <div className="flex items-center justify-end gap-2">
        {importResult && (
          <span className="mr-auto text-[11px] text-muted-foreground">
            Imported {importResult.imported} memories{importResult.skipped > 0 ? `, skipped ${importResult.skipped} duplicates` : ""}.
          </span>
        )}
        {importError && (
          <span className="mr-auto text-[11px] text-destructive">{importError}</span>
        )}
        <Button size="sm" variant="ghost" className="h-7 gap-1.5 text-xs" onClick={handleExport}>
          <Download className="h-3.5 w-3.5" />
          Export
        </Button>
        {canEdit && (
          <label className="inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-md px-2 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground">
            {importing ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Upload className="h-3.5 w-3.5" />}
            Import
            <input type="file" accept=".json" className="hidden" disabled={importing || !!pendingImport} onChange={handleImport} />
          </label>
        )}
      </div>

      {pendingImport && (
        <div className="rounded-lg border bg-muted/40 p-4">
          <p className="mb-1 text-sm font-medium">Map imported users</p>
          <p className="mb-3 text-xs text-muted-foreground">
            This bundle contains user preferences from {pendingImport.unmappedEmails.length === 1 ? "an external user" : "external users"}.
            Map each to a workspace member so preferences are attributed correctly, or leave unassigned.
          </p>
          <div className="mb-4 flex flex-col gap-2">
            {pendingImport.unmappedEmails.map((email) => (
              <div key={email} className="flex items-center gap-3">
                <span className="w-0 flex-1 truncate text-xs text-muted-foreground" title={email}>{email}</span>
                <span className="text-xs text-muted-foreground">→</span>
                <select
                  className="h-7 rounded-md border bg-background px-2 text-xs"
                  value={pendingImport.mappings[email] ?? ""}
                  onChange={(e) =>
                    setPendingImport((p) => p && ({
                      ...p,
                      mappings: { ...p.mappings, [email]: e.target.value },
                    }))
                  }
                >
                  <option value="">Leave unassigned</option>
                  {(members ?? []).map((m) => (
                    <option key={m.user_id} value={m.user_id}>
                      {m.name || m.email}
                    </option>
                  ))}
                </select>
              </div>
            ))}
          </div>
          <div className="flex justify-end gap-2">
            <Button size="sm" variant="ghost" onClick={() => setPendingImport(null)}>Cancel</Button>
            <Button size="sm" disabled={importing} onClick={() => doImport(pendingImport.bundle, pendingImport.mappings)}>
              {importing ? <><Loader2 className="mr-1.5 h-3 w-3 animate-spin" />Importing…</> : "Confirm Import"}
            </Button>
          </div>
        </div>
      )}

      <MoodSection persona={persona} />
      <TraitsSection persona={persona} canEdit={canEdit} onSave={(data) => mutation.mutate(data)} saving={mutation.isPending} />
      <StrengthsSection persona={persona} canEdit={canEdit} onSave={(data) => mutation.mutate(data)} />
      <IdentitySection persona={persona} canEdit={canEdit} onSave={(data) => mutation.mutate(data)} saving={mutation.isPending} />
      <VarianceSection persona={persona} canEdit={canEdit} onSave={(data) => mutation.mutate(data)} saving={mutation.isPending} />
      <SynthesizeSection
        persona={persona}
        canEdit={canEdit}
        onSynthesize={() => synthesize.mutate()}
        synthesizing={synthesize.isPending}
        error={synthesisError}
        backendConfigured={personaSynthesisBackend !== ""}
      />
      {persona.recent_signals.length > 0 && <SignalsSection persona={persona} />}
      <EpisodeRecallSection persona={persona} canEdit={canEdit} onSave={(data) => mutation.mutate(data)} saving={mutation.isPending} />
      <div className="flex flex-col gap-2">
        <MemoriesSection agentId={agent.id} workspaceId={agent.workspace_id} canEdit={canEdit} />
        <LLMCallsSection agentId={agent.id} />
      </div>
    </div>
  );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{children}</h3>;
}

function MoodSection({ persona }: { persona: AgentPersona }) {
  const label = MOOD_LABELS[persona.mood] ?? persona.mood;
  const colorClass = MOOD_COLORS[persona.mood] ?? "bg-muted text-muted-foreground";
  return (
    <div className="flex items-center gap-3">
      <SectionTitle>Current Mood</SectionTitle>
      <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium ${colorClass}`}>
        <Sparkles className="h-3 w-3" />
        {label}
      </span>
      <span className="ml-auto text-xs text-muted-foreground">
        {persona.signal_count} signal{persona.signal_count !== 1 ? "s" : ""} accumulated
      </span>
    </div>
  );
}

function TraitsSection({
  persona,
  canEdit,
  onSave,
  saving,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSave: (data: UpdateAgentPersonaRequest) => void;
  saving: boolean;
}) {
  const [local, setLocal] = useState<Record<string, number>>(() =>
    Object.fromEntries(TRAITS.map((t) => [t.key, persona[t.key] as number])),
  );
  const isDirty = TRAITS.some((t) => local[t.key] !== persona[t.key]);

  const handleSave = () => {
    const patch: UpdateAgentPersonaRequest = {};
    for (const t of TRAITS) {
      if (local[t.key] !== persona[t.key]) {
        (patch as Record<string, number>)[t.key] = local[t.key] ?? 50;
      }
    }
    onSave(patch);
  };

  return (
    <div className="flex flex-col gap-3">
      <SectionTitle>Personality Traits</SectionTitle>
      <div className="flex flex-col gap-4 rounded-lg border bg-muted/30 p-4">
        {TRAITS.map((trait) => (
          <div key={trait.key} className="flex flex-col gap-1.5">
            <div className="flex items-center justify-between">
              <span className="text-xs font-medium">{trait.label}</span>
              <span className="text-xs tabular-nums text-muted-foreground">{local[trait.key]}</span>
            </div>
            <input
              type="range"
              min={0}
              max={100}
              value={local[trait.key]}
              disabled={!canEdit}
              onChange={(e) =>
                setLocal((prev) => ({ ...prev, [trait.key]: Number(e.target.value) }))
              }
              className="h-1.5 w-full cursor-pointer appearance-none rounded-full bg-border accent-foreground disabled:cursor-default"
            />
            <div className="flex justify-between text-[10px] text-muted-foreground">
              <span>{trait.lowLabel}</span>
              <span>{trait.highLabel}</span>
            </div>
          </div>
        ))}
      </div>
      {canEdit && isDirty && (
        <div className="flex justify-end">
          <Button size="sm" onClick={handleSave} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
            Save traits
          </Button>
        </div>
      )}
    </div>
  );
}

function StrengthsSection({
  persona,
  canEdit,
  onSave,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSave: (data: UpdateAgentPersonaRequest) => void;
}) {
  const [strengths, setStrengths] = useState(persona.strengths);
  const [blindSpots, setBlindSpots] = useState(persona.blind_spots);
  const [newStrength, setNewStrength] = useState("");
  const [newBlindSpot, setNewBlindSpot] = useState("");

  const isDirty =
    JSON.stringify(strengths) !== JSON.stringify(persona.strengths) ||
    JSON.stringify(blindSpots) !== JSON.stringify(persona.blind_spots);

  const handleSave = () => {
    onSave({ strengths, blind_spots: blindSpots });
  };

  const addItem = (list: string[], setList: (v: string[]) => void, value: string, setValue: (v: string) => void) => {
    const trimmed = value.trim();
    if (trimmed && !list.includes(trimmed)) {
      setList([...list, trimmed]);
    }
    setValue("");
  };

  const removeItem = (list: string[], setList: (v: string[]) => void, index: number) => {
    setList(list.filter((_, i) => i !== index));
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="grid grid-cols-2 gap-4">
        <div className="flex flex-col gap-2">
          <SectionTitle>Strengths</SectionTitle>
          <div className="flex flex-wrap gap-1.5 min-h-[28px]">
            {strengths.map((s, i) => (
              <Badge key={i} variant="secondary" className="gap-1 pr-1 text-xs">
                {s}
                {canEdit && (
                  <button onClick={() => removeItem(strengths, setStrengths, i)} className="ml-0.5 rounded hover:text-destructive">
                    <X className="h-2.5 w-2.5" />
                  </button>
                )}
              </Badge>
            ))}
          </div>
          {canEdit && (
            <div className="flex gap-1.5">
              <Input
                value={newStrength}
                onChange={(e) => setNewStrength(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && addItem(strengths, setStrengths, newStrength, setNewStrength)}
                placeholder="Add strength…"
                className="h-7 text-xs"
              />
              <Button size="icon" variant="outline" className="h-7 w-7 shrink-0" onClick={() => addItem(strengths, setStrengths, newStrength, setNewStrength)}>
                <Plus className="h-3.5 w-3.5" />
              </Button>
            </div>
          )}
        </div>
        <div className="flex flex-col gap-2">
          <SectionTitle>Blind Spots</SectionTitle>
          <div className="flex flex-wrap gap-1.5 min-h-[28px]">
            {blindSpots.map((s, i) => (
              <Badge key={i} variant="outline" className="gap-1 pr-1 text-xs text-muted-foreground">
                {s}
                {canEdit && (
                  <button onClick={() => removeItem(blindSpots, setBlindSpots, i)} className="ml-0.5 rounded hover:text-destructive">
                    <X className="h-2.5 w-2.5" />
                  </button>
                )}
              </Badge>
            ))}
          </div>
          {canEdit && (
            <div className="flex gap-1.5">
              <Input
                value={newBlindSpot}
                onChange={(e) => setNewBlindSpot(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && addItem(blindSpots, setBlindSpots, newBlindSpot, setNewBlindSpot)}
                placeholder="Add blind spot…"
                className="h-7 text-xs"
              />
              <Button size="icon" variant="outline" className="h-7 w-7 shrink-0" onClick={() => addItem(blindSpots, setBlindSpots, newBlindSpot, setNewBlindSpot)}>
                <Plus className="h-3.5 w-3.5" />
              </Button>
            </div>
          )}
        </div>
      </div>
      {canEdit && isDirty && (
        <div className="flex justify-end">
          <Button size="sm" onClick={handleSave}>
            <Save className="mr-1.5 h-3.5 w-3.5" />
            Save
          </Button>
        </div>
      )}
    </div>
  );
}

function IdentitySection({
  persona,
  canEdit,
  onSave,
  saving,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSave: (data: UpdateAgentPersonaRequest) => void;
  saving: boolean;
}) {
  const [value, setValue] = useState(persona.identity ?? "");
  const isDirty = value !== (persona.identity ?? "");

  return (
    <div className="flex flex-col gap-2">
      <SectionTitle>Self-Identity</SectionTitle>
      <p className="text-xs text-muted-foreground">
        How this agent narrates its own character. Updated as its personality evolves.
      </p>
      <Textarea
        value={value}
        onChange={(e) => setValue(e.target.value)}
        disabled={!canEdit}
        placeholder="I'm the kind of agent who…"
        className="min-h-[80px] resize-none text-sm"
      />
      {canEdit && isDirty && (
        <div className="flex justify-end">
          <Button size="sm" onClick={() => onSave({ identity: value })} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
            Save
          </Button>
        </div>
      )}
    </div>
  );
}

function VarianceSection({
  persona,
  canEdit,
  onSave,
  saving,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSave: (data: UpdateAgentPersonaRequest) => void;
  saving: boolean;
}) {
  const [value, setValue] = useState(persona.variance_level);
  const isDirty = value !== persona.variance_level;

  return (
    <div className="flex flex-col gap-2">
      <SectionTitle>Spontaneity</SectionTitle>
      <p className="text-xs text-muted-foreground">
        How much personality variance shows up in each response. Higher = occasional "sparks" of character.
      </p>
      <div className="flex items-center gap-3 rounded-lg border bg-muted/30 px-4 py-3">
        <span className="w-20 text-xs text-muted-foreground">Consistent</span>
        <input
          type="range"
          min={0}
          max={100}
          value={value}
          disabled={!canEdit}
          onChange={(e) => setValue(Number(e.target.value))}
          className="flex-1 h-1.5 cursor-pointer appearance-none rounded-full bg-border accent-foreground disabled:cursor-default"
        />
        <span className="w-20 text-right text-xs text-muted-foreground">Spontaneous</span>
        <span className="w-8 text-right text-xs tabular-nums font-medium">{value}</span>
      </div>
      {canEdit && isDirty && (
        <div className="flex justify-end">
          <Button size="sm" onClick={() => onSave({ variance_level: value })} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
            Save
          </Button>
        </div>
      )}
    </div>
  );
}

function EpisodeRecallSection({
  persona,
  canEdit,
  onSave,
  saving,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSave: (data: UpdateAgentPersonaRequest) => void;
  saving: boolean;
}) {
  const [value, setValue] = useState(persona.episode_recall_count ?? 5);
  const isDirty = value !== (persona.episode_recall_count ?? 5);

  return (
    <div className="flex flex-col gap-2">
      <SectionTitle>Recent Message Window</SectionTitle>
      <p className="text-xs text-muted-foreground">
        Controls how many recent chat messages (×4) are injected into each task brief as verbatim working memory — the agent's "what did we just talk about?" layer.{" "}
        <span className="text-foreground/60">Recommended range: 3–8. Each unit ≈ 4 messages. Higher values give broader recent context but consume more of the context window; lower values keep the brief lean but may miss recent exchanges.</span>
      </p>
      <div className="flex items-center gap-3 rounded-lg border bg-muted/30 px-4 py-3">
        <input
          type="number"
          min={1}
          max={20}
          value={value}
          disabled={!canEdit}
          onChange={(e) => {
            const n = Math.max(1, Math.min(20, Number(e.target.value)));
            setValue(n);
          }}
          className="w-16 rounded-md border bg-background px-2 py-1 text-center text-sm tabular-nums disabled:cursor-default disabled:opacity-60"
        />
        <span className="text-xs text-muted-foreground">units (1–20, ≈ 4 msgs each)</span>
      </div>
      {canEdit && isDirty && (
        <div className="flex justify-end">
          <Button size="sm" onClick={() => onSave({ episode_recall_count: value })} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
            Save
          </Button>
        </div>
      )}
    </div>
  );
}

function SynthesizeSection({
  persona,
  canEdit,
  onSynthesize,
  synthesizing,
  error,
  backendConfigured,
}: {
  persona: AgentPersona;
  canEdit: boolean;
  onSynthesize: () => void;
  synthesizing: boolean;
  error: string | null;
  backendConfigured: boolean;
}) {
  const lastSynth = persona.last_synthesized_at
    ? new Date(persona.last_synthesized_at).toLocaleString()
    : null;

  return (
    <div className="flex flex-col gap-2">
      <SectionTitle>Instructions Synthesis</SectionTitle>
      {!backendConfigured && (
        <p className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-[11px] text-amber-800 dark:border-amber-800/40 dark:bg-amber-950/30 dark:text-amber-400">
          No LLM backend configured — synthesis and comment classification will use keyword matching only.
          Set <code className="font-mono">ANTHROPIC_API_KEY</code> or{" "}
          <code className="font-mono">PERSONA_SYNTHESIS_BASE_URL</code> on the server to enable full LLM support.
        </p>
      )}
      <div className="flex items-center gap-3 rounded-lg border bg-muted/30 px-4 py-3">
        <div className="min-w-0 flex-1">
          <p className="text-xs text-foreground">
            Re-generate this agent&apos;s instructions from its current persona data using an LLM.{" "}
            <span className="text-muted-foreground">Also runs automatically every 15 feedback signals.</span>
          </p>
          {lastSynth ? (
            <p className="mt-0.5 text-[10px] text-muted-foreground">Last synthesized: {lastSynth}</p>
          ) : (
            <p className="mt-0.5 text-[10px] text-muted-foreground">Never synthesized yet</p>
          )}
        </div>
        {canEdit && (
          <Button
            size="sm"
            variant="outline"
            onClick={onSynthesize}
            disabled={synthesizing || !backendConfigured}
            className="shrink-0"
          >
            {synthesizing ? (
              <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
            ) : (
              <Wand2 className="mr-1.5 h-3.5 w-3.5" />
            )}
            {synthesizing ? "Synthesizing…" : "Synthesize"}
          </Button>
        )}
      </div>
      {error && (
        <p className="text-[11px] text-destructive">{error}</p>
      )}
    </div>
  );
}

function fmtDatetime(s: string | Date): string {
  const d = typeof s === "string" ? new Date(s) : s;
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

const SENTIMENT_COLORS: Record<string, string> = {
  positive: "text-emerald-500",
  negative: "text-rose-500",
  neutral: "text-muted-foreground",
};

const SENTIMENT_LABELS: Record<string, string> = {
  positive: "✓",
  negative: "✗",
  neutral: "·",
};

function MemoryRow({
  memory,
  agentId,
  canEdit,
  showSentiment,
}: {
  memory: AgentMemory;
  agentId: string;
  canEdit: boolean;
  showSentiment: boolean;
}) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(memory.content);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const del = useMutation({
    mutationFn: () => api.deleteAgentMemory(agentId, memory.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["agent-memories", agentId] }),
  });

  const update = useMutation({
    mutationFn: (content: string) => api.updateAgentMemory(agentId, memory.id, content),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["agent-memories", agentId] });
      setEditing(false);
    },
  });

  const handleSave = () => {
    const trimmed = draft.trim();
    if (trimmed && trimmed !== memory.content) {
      update.mutate(trimmed);
    } else {
      setEditing(false);
    }
  };

  const handleCancelEdit = () => {
    setDraft(memory.content);
    setEditing(false);
  };

  return (
    <div className="group flex items-start gap-2.5 border-b px-3 py-2.5 last:border-b-0">
      {showSentiment && (
        <span className={`mt-0.5 shrink-0 text-xs font-bold ${SENTIMENT_COLORS[memory.sentiment]}`}>
          {SENTIMENT_LABELS[memory.sentiment]}
        </span>
      )}
      <div className="min-w-0 flex-1">
        {editing ? (
          <div className="flex flex-col gap-1.5">
            <Textarea
              className="resize-none text-xs"
              rows={3}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              autoFocus
            />
            <div className="flex items-center gap-1.5">
              <Button size="sm" onClick={handleSave} disabled={update.isPending}>
                {update.isPending ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
                Save
              </Button>
              <Button size="sm" variant="ghost" onClick={handleCancelEdit} disabled={update.isPending}>
                Cancel
              </Button>
              {update.isError && <span className="text-xs text-destructive">Save failed</span>}
            </div>
          </div>
        ) : (
          <p className="text-xs text-foreground">{memory.content}</p>
        )}
        <div className="mt-0.5 flex items-center gap-2 text-[10px] text-muted-foreground">
          {showSentiment && (
            <>
              <span className="capitalize">{memory.category.replace(/_/g, " ")}</span>
              <span>·</span>
            </>
          )}
          <span>{fmtDatetime(memory.created_at)}</span>
          {memory.has_embedding && <span className="rounded bg-muted px-1 py-px font-mono">vec</span>}
          {memory.is_consolidated && memory.source_count > 1 && (
            <span className="rounded bg-violet-100 px-1 py-px text-violet-700 dark:bg-violet-950/50 dark:text-violet-400">
              merged from {memory.source_count}
            </span>
          )}
          {showSentiment && <span className="ml-auto">imp {memory.importance.toFixed(2)}</span>}
        </div>
      </div>
      {canEdit && !editing && (
        <div className="flex shrink-0 items-center gap-0.5 opacity-0 transition-opacity group-hover:opacity-100">
          <Button
            variant="ghost"
            size="icon-sm"
            title="Edit"
            className="text-muted-foreground hover:text-foreground"
            onClick={() => { setDraft(memory.content); setEditing(true); setConfirmDelete(false); }}
          >
            <Pencil className="h-3.5 w-3.5" />
          </Button>
          {confirmDelete ? (
            <>
              <Button
                variant="ghost"
                size="icon-sm"
                title="Confirm delete"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                onClick={() => del.mutate()}
                disabled={del.isPending}
              >
                {del.isPending ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Check className="h-3.5 w-3.5" />}
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                title="Cancel"
                className="text-muted-foreground hover:text-foreground"
                onClick={() => setConfirmDelete(false)}
              >
                <X className="h-3.5 w-3.5" />
              </Button>
            </>
          ) : (
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-destructive"
              title="Delete"
              onClick={() => setConfirmDelete(true)}
            >
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

function MemoriesSection({
  agentId,
  workspaceId,
  canEdit,
}: {
  agentId: string;
  workspaceId: string;
  canEdit: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [prefOpen, setPrefOpen] = useState(false);
  const [lastRebuiltAt, setLastRebuiltAt] = useState<Date | null>(null);
  const embeddingModelStale = useConfigStore((s) => s.embeddingModelStale);
  const setEmbeddingModelStale = useConfigStore((s) => s.setEmbeddingModelStale);

  const { data: memories, isLoading } = useQuery({
    queryKey: ["agent-memories", agentId],
    queryFn: () => api.listAgentMemories(agentId),
    enabled: open || prefOpen,
  });

  const rebuild = useMutation({
    mutationFn: () => api.rebuildWorkspaceEmbeddings(workspaceId),
    onSuccess: () => { setTimeout(() => rebuild.reset(), 4000); },
    onError: () => { setTimeout(() => rebuild.reset(), 5000); },
  });

  useWSEvent("memory:embeddings_rebuilt", () => {
    setLastRebuiltAt(new Date());
    setEmbeddingModelStale(false);
  });

  const episodicMemories = memories?.filter((m: AgentMemory) => m.category !== "user_preference");
  const preferenceMemories = memories?.filter((m: AgentMemory) => m.category === "user_preference");

  return (
    <div className="flex flex-col gap-2">
      {/* Embeddings — always-visible header with rebuild action */}
      <div className="flex flex-col gap-2 rounded-lg border px-3 py-3">
        <div className="flex items-center gap-2">
          <Cpu className="h-3.5 w-3.5 text-muted-foreground" />
          <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Embeddings</span>
          {canEdit && (
            <Button
              size="sm"
              variant={rebuild.isError ? "destructive" : "outline"}
              className="ml-auto shrink-0"
              disabled={rebuild.isPending || rebuild.isSuccess}
              onClick={() => rebuild.mutate()}
            >
              {rebuild.isPending ? (
                <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
              ) : (
                <RefreshCw className="mr-1.5 h-3.5 w-3.5" />
              )}
              {rebuild.isPending ? "Rebuilding…" : rebuild.isSuccess ? "Queued" : rebuild.isError ? "Failed — retry?" : "Rebuild"}
            </Button>
          )}
        </div>
        <p className="text-xs text-muted-foreground">
          Each memory carries a vector embedding used for semantic recall during tasks. Rebuild if you
          changed the embedding model or imported memories from another instance.{" "}
          <span className="text-foreground/50">
            Runs in the background — refresh the page after a minute to see updated results.
          </span>
        </p>
        {embeddingModelStale && canEdit && (
          <p className="text-[11px] text-amber-600 dark:text-amber-400">
            Embedding model changed — existing vectors are stale and recall may be inaccurate.
          </p>
        )}
        {lastRebuiltAt && (
          <p className="text-[10px] text-muted-foreground/60">
            Last rebuild completed at {lastRebuiltAt.toLocaleTimeString()}
          </p>
        )}
      </div>

      {/* Episodic Memory */}
      <button
        className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground hover:text-foreground"
        onClick={() => setOpen((v) => !v)}
      >
        <Brain className="h-3.5 w-3.5" />
        Episodic Memory
        {open ? <ChevronDown className="ml-auto h-3 w-3" /> : <ChevronRight className="ml-auto h-3 w-3" />}
      </button>
      {open && (
        <div className="flex flex-col rounded-lg border">
          {isLoading && (
            <div className="flex items-center justify-center py-6">
              <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
            </div>
          )}
          {!isLoading && (!episodicMemories || episodicMemories.length === 0) && (
            <p className="px-3 py-4 text-xs text-muted-foreground">No memories yet. Memories are recorded automatically after tasks complete.</p>
          )}
          {episodicMemories && episodicMemories.length > 0 && episodicMemories.map((m: AgentMemory) => (
            <MemoryRow key={m.id} memory={m} agentId={agentId} canEdit={canEdit} showSentiment />
          ))}
        </div>
      )}

      {/* User Preferences */}
      <button
        className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground hover:text-foreground"
        onClick={() => setPrefOpen((v) => !v)}
      >
        <UserRound className="h-3.5 w-3.5" />
        User Preferences
        {prefOpen ? <ChevronDown className="ml-auto h-3 w-3" /> : <ChevronRight className="ml-auto h-3 w-3" />}
      </button>
      {prefOpen && (
        <div className="flex flex-col rounded-lg border">
          {isLoading && (
            <div className="flex items-center justify-center py-6">
              <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
            </div>
          )}
          {!isLoading && (!preferenceMemories || preferenceMemories.length === 0) && (
            <p className="px-3 py-4 text-xs text-muted-foreground">No user preferences recorded yet. Preferences are learned from interactions with workspace members.</p>
          )}
          {preferenceMemories && preferenceMemories.length > 0 && preferenceMemories.map((m: AgentMemory) => (
            <MemoryRow key={m.id} memory={m} agentId={agentId} canEdit={canEdit} showSentiment={false} />
          ))}
        </div>
      )}
    </div>
  );
}

const CALL_TYPE_LABELS: Record<string, string> = {
  synthesis: "Instruction synthesis",
  classification: "Signal classification",
  compaction: "Memory compaction",
  emotional_impression: "Emotional impression",
  breakthrough_impression: "Breakthrough impression",
  user_preference: "User preference",
  task_summary: "Task outcome memory",
  episode_summary: "Episode summary",
};

type AggView = "calls" | "day" | "week" | "month";

const AGG_VIEW_LABELS: Record<AggView, string> = {
  calls: "Calls",
  day: "Daily",
  week: "Weekly",
  month: "Monthly",
};

function groupByPeriod(calls: PersonaLLMCall[], by: "day" | "week" | "month") {
  const groups = new Map<string, { label: string; count: number; inputTokens: number; outputTokens: number }>();
  for (const c of calls) {
    const d = new Date(c.created_at);
    let key: string;
    let label: string;
    if (by === "day") {
      key = d.toISOString().slice(0, 10);
      label = d.toLocaleString(undefined, { month: "short", day: "numeric" });
    } else if (by === "week") {
      const mon = new Date(d);
      mon.setDate(d.getDate() - ((d.getDay() + 6) % 7));
      mon.setHours(0, 0, 0, 0);
      const sun = new Date(mon);
      sun.setDate(mon.getDate() + 6);
      key = mon.toISOString().slice(0, 10);
      const fmt = (dt: Date) => dt.toLocaleString(undefined, { month: "short", day: "numeric" });
      label = `${fmt(mon)} – ${fmt(sun)}`;
    } else {
      key = d.toISOString().slice(0, 7);
      label = d.toLocaleString(undefined, { month: "short", year: "numeric" });
    }
    const g = groups.get(key) ?? { label, count: 0, inputTokens: 0, outputTokens: 0 };
    g.count++;
    g.inputTokens += c.input_tokens;
    g.outputTokens += c.output_tokens;
    groups.set(key, g);
  }
  return Array.from(groups.values());
}

function LLMCallsSection({ agentId }: { agentId: string }) {
  const [open, setOpen] = useState(false);
  const [view, setView] = useState<AggView>("calls");

  const { data: calls, isLoading } = useQuery({
    queryKey: ["agent-llm-calls", agentId],
    queryFn: () => api.listAgentLLMCalls(agentId),
    enabled: open,
  });

  const totalIn = calls?.reduce((s, c) => s + c.input_tokens, 0) ?? 0;
  const totalOut = calls?.reduce((s, c) => s + c.output_tokens, 0) ?? 0;
  const grouped = calls && view !== "calls" ? groupByPeriod(calls, view) : null;

  return (
    <div className="flex flex-col gap-2">
      <button
        className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground hover:text-foreground"
        onClick={() => setOpen((v) => !v)}
      >
        <Activity className="h-3.5 w-3.5" />
        LLM Token Usage
        {open ? <ChevronDown className="ml-auto h-3 w-3" /> : <ChevronRight className="ml-auto h-3 w-3" />}
      </button>
      {open && (
        <div className="flex flex-col rounded-lg border">
          <p className="border-b px-3 py-2 text-[10px] text-muted-foreground">
            Token usage from persona background processes: signal classification, instruction synthesis, memory compaction, and episode summaries. Does not include tokens used by the agent during task execution.
          </p>
          {isLoading && (
            <div className="flex items-center justify-center py-6">
              <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
            </div>
          )}
          {!isLoading && (!calls || calls.length === 0) && (
            <p className="px-3 py-4 text-xs text-muted-foreground">No LLM calls recorded yet.</p>
          )}
          {calls && calls.length > 0 && (
            <>
              <div className="flex items-center gap-3 border-b px-3 py-2">
                <span className="text-[10px] tabular-nums text-muted-foreground">
                  {(totalIn + totalOut).toLocaleString()} tokens
                  <span className="ml-1.5 opacity-60">({totalIn.toLocaleString()} in / {totalOut.toLocaleString()} out)</span>
                </span>
                <div className="ml-auto flex gap-0.5">
                  {(["calls", "day", "week", "month"] as const).map((v) => (
                    <button
                      key={v}
                      onClick={() => setView(v)}
                      className={`rounded px-2 py-0.5 text-[10px] transition-colors ${view === v ? "bg-accent text-accent-foreground" : "text-muted-foreground hover:text-foreground"}`}
                    >
                      {AGG_VIEW_LABELS[v]}
                    </button>
                  ))}
                </div>
              </div>
              {view === "calls" && calls.map((c: PersonaLLMCall, i: number) => (
                <div key={i} className="flex items-center gap-2.5 border-b px-3 py-2 last:border-b-0">
                  <span className="w-24 shrink-0 text-[10px] font-medium text-muted-foreground">
                    {CALL_TYPE_LABELS[c.call_type] ?? c.call_type}
                  </span>
                  <span className="min-w-0 flex-1 truncate font-mono text-[10px] text-foreground/70">{c.model}</span>
                  <span className="shrink-0 tabular-nums text-[10px] text-muted-foreground">
                    {c.input_tokens.toLocaleString()}↑ {c.output_tokens.toLocaleString()}↓
                  </span>
                  <span className="shrink-0 text-[10px] text-muted-foreground/60">{c.latency_ms}ms</span>
                  <span className="shrink-0 text-[10px] text-muted-foreground/50">{fmtDatetime(c.created_at)}</span>
                </div>
              ))}
              {grouped && grouped.map((g, i) => (
                <div key={i} className="flex items-center gap-2.5 border-b px-3 py-2 last:border-b-0">
                  <span className="min-w-0 flex-1 text-[10px] font-medium text-foreground">{g.label}</span>
                  <span className="shrink-0 text-[10px] text-muted-foreground/60">{g.count} calls</span>
                  <span className="shrink-0 tabular-nums text-[10px] text-muted-foreground">
                    {g.inputTokens.toLocaleString()}↑ {g.outputTokens.toLocaleString()}↓
                  </span>
                </div>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}

function SignalsSection({ persona }: { persona: AgentPersona }) {
  return (
    <div className="flex flex-col gap-2 pb-4">
      <SectionTitle>Recent Feedback Signals</SectionTitle>
      <div className="flex flex-col divide-y rounded-lg border">
        {persona.recent_signals.slice(0, 10).map((signal) => (
          <div key={signal.id} className="flex items-start gap-2.5 px-3 py-2.5">
            <span className="mt-0.5 shrink-0">{SIGNAL_ICONS[signal.type] ?? null}</span>
            <div className="min-w-0 flex-1">
              <p className="truncate text-xs text-foreground">{signal.content}</p>
              <p className="mt-0.5 text-[10px] text-muted-foreground capitalize">
                {signal.type.replace(/_/g, " ")} · {fmtDatetime(signal.created_at)}
              </p>
            </div>
            <span className="shrink-0 text-[10px] tabular-nums text-muted-foreground">
              ×{signal.weight.toFixed(1)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
