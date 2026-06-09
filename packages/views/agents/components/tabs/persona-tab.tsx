"use client";

import { useCallback, useState } from "react";
import { Loader2, Save, Sparkles, ThumbsUp, ThumbsDown, Plus, X, Wand2, Brain, ChevronDown, ChevronRight, Cpu, UserRound, Download, Upload } from "lucide-react";
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
    setImporting(true);
    setImportResult(null);
    try {
      const result = await api.importAgentPersona(agent.id, file);
      setImportResult({ imported: result.memories_imported, skipped: result.memories_skipped });
      qc.invalidateQueries({ queryKey: ["agent-memories", agent.id] });
      qc.invalidateQueries({ queryKey });
    } finally {
      setImporting(false);
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
        <Button size="sm" variant="ghost" className="h-7 gap-1.5 text-xs" onClick={handleExport}>
          <Download className="h-3.5 w-3.5" />
          Export
        </Button>
        {canEdit && (
          <label className="inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-md px-2 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground">
            {importing ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Upload className="h-3.5 w-3.5" />}
            Import
            <input type="file" accept=".json" className="hidden" disabled={importing} onChange={handleImport} />
          </label>
        )}
      </div>
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
      <MemoriesSection agentId={agent.id} workspaceId={agent.workspace_id} canEdit={canEdit} />
      <LLMCallsSection agentId={agent.id} />
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
            Re-generate this agent&apos;s instructions from its current persona data using an LLM.
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
  const [prefOpen, setPrefOpen] = useState(true);
  const embeddingModelStale = useConfigStore((s) => s.embeddingModelStale);

  const { data: memories, isLoading } = useQuery({
    queryKey: ["agent-memories", agentId],
    queryFn: () => api.listAgentMemories(agentId),
    enabled: open || prefOpen,
  });

  const rebuild = useMutation({
    mutationFn: () => api.rebuildWorkspaceEmbeddings(workspaceId),
  });

  const episodicMemories = memories?.filter((m: AgentMemory) => m.category !== "user_preference");
  const preferenceMemories = memories?.filter((m: AgentMemory) => m.category === "user_preference");

  return (
    <div className="flex flex-col gap-2 pb-4">
      {embeddingModelStale && canEdit && (
        <div className="flex items-center justify-between gap-3 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2.5 text-xs dark:border-amber-900/50 dark:bg-amber-950/30">
          <span className="text-amber-800 dark:text-amber-300">
            Embedding model changed — existing memory vectors are stale. Semantic recall will be inaccurate until embeddings are rebuilt.
          </span>
          <Button
            size="sm"
            variant="outline"
            className="shrink-0 border-amber-300 text-amber-800 hover:bg-amber-100 dark:border-amber-700 dark:text-amber-300 dark:hover:bg-amber-900/40"
            disabled={rebuild.isPending || rebuild.isSuccess}
            onClick={() => rebuild.mutate()}
          >
            {rebuild.isPending ? (
              <><Loader2 className="mr-1.5 h-3 w-3 animate-spin" />Rebuilding…</>
            ) : rebuild.isSuccess ? (
              "Started"
            ) : (
              "Rebuild"
            )}
          </Button>
        </div>
      )}
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
            <div key={m.id} className="flex items-start gap-2.5 border-b px-3 py-2.5 last:border-b-0">
              <span className={`mt-0.5 shrink-0 text-xs font-bold ${SENTIMENT_COLORS[m.sentiment]}`}>
                {SENTIMENT_LABELS[m.sentiment]}
              </span>
              <div className="min-w-0 flex-1">
                <p className="text-xs text-foreground">{m.content}</p>
                <div className="mt-0.5 flex items-center gap-2 text-[10px] text-muted-foreground">
                  <span className="capitalize">{m.category.replace(/_/g, " ")}</span>
                  <span>·</span>
                  <span>{new Date(m.created_at).toLocaleDateString()}</span>
                  {m.has_embedding && <span className="rounded bg-muted px-1 py-px font-mono">vec</span>}
                  {m.is_consolidated && m.source_count > 1 && (
                    <span className="rounded bg-violet-100 px-1 py-px text-violet-700 dark:bg-violet-950/50 dark:text-violet-400">
                      merged from {m.source_count}
                    </span>
                  )}
                  <span className="ml-auto">imp {m.importance.toFixed(2)}</span>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
      {preferenceMemories && preferenceMemories.length > 0 && (
        <>
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
              {preferenceMemories.map((m: AgentMemory) => (
                <div key={m.id} className="flex items-start gap-2.5 border-b px-3 py-2.5 last:border-b-0">
                  <div className="min-w-0 flex-1">
                    <p className="text-xs text-foreground">{m.content}</p>
                    <div className="mt-0.5 flex items-center gap-2 text-[10px] text-muted-foreground">
                      <span>{new Date(m.created_at).toLocaleDateString()}</span>
                      {m.has_embedding && <span className="rounded bg-muted px-1 py-px font-mono">vec</span>}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}

const CALL_TYPE_LABELS: Record<string, string> = {
  synthesis: "Synthesis",
  classification: "Classification",
  compaction: "Compaction",
  emotional_impression: "Emotional",
  breakthrough_impression: "Breakthrough",
};

function LLMCallsSection({ agentId }: { agentId: string }) {
  const [open, setOpen] = useState(false);

  const { data: calls, isLoading } = useQuery({
    queryKey: ["agent-llm-calls", agentId],
    queryFn: () => api.listAgentLLMCalls(agentId),
    enabled: open,
  });

  const totalIn = calls?.reduce((s, c) => s + c.input_tokens, 0) ?? 0;
  const totalOut = calls?.reduce((s, c) => s + c.output_tokens, 0) ?? 0;

  return (
    <div className="flex flex-col gap-2 pb-4">
      <button
        className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground hover:text-foreground"
        onClick={() => setOpen((v) => !v)}
      >
        <Cpu className="h-3.5 w-3.5" />
        LLM Token Usage
        {open ? <ChevronDown className="ml-auto h-3 w-3" /> : <ChevronRight className="ml-auto h-3 w-3" />}
      </button>
      {open && (
        <div className="flex flex-col rounded-lg border">
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
              <div className="flex items-center gap-4 border-b px-3 py-2 text-[10px] text-muted-foreground">
                <span>Last {calls.length} calls</span>
                <span className="ml-auto tabular-nums">
                  {(totalIn + totalOut).toLocaleString()} tokens total
                  <span className="ml-2 opacity-70">({totalIn.toLocaleString()} in / {totalOut.toLocaleString()} out)</span>
                </span>
              </div>
              {calls.map((c: PersonaLLMCall, i: number) => (
                <div key={i} className="flex items-center gap-2.5 border-b px-3 py-2 last:border-b-0">
                  <span className="w-24 shrink-0 text-[10px] font-medium text-muted-foreground">
                    {CALL_TYPE_LABELS[c.call_type] ?? c.call_type}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-[10px] text-foreground/70 font-mono">{c.model}</span>
                  <span className="shrink-0 tabular-nums text-[10px] text-muted-foreground">
                    {c.input_tokens.toLocaleString()}↑ {c.output_tokens.toLocaleString()}↓
                  </span>
                  <span className="shrink-0 text-[10px] text-muted-foreground/60">
                    {c.latency_ms}ms
                  </span>
                  <span className="shrink-0 text-[10px] text-muted-foreground/50">
                    {new Date(c.created_at).toLocaleDateString()}
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
                {signal.type.replace(/_/g, " ")} · {new Date(signal.created_at).toLocaleDateString()}
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
