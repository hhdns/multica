import { createStore } from "zustand/vanilla";
import { useStore } from "zustand";

interface ConfigState {
  cdnDomain: string;
  // True when cdnDomain serves private content via time-bounded signed URLs
  // (CloudFront signing enabled server-side). Renderers must not treat a raw
  // storage URL on that domain as a loadable media source (MUL-3254).
  cdnSigned: boolean;
  allowSignup: boolean;
  googleClientId: string;
  daemonServerUrl: string;
  daemonAppUrl: string;
  // Self-host gate (#3433): when true, every "Create workspace" affordance
  // must be hidden. Defaults to false so unknown / older servers behave like
  // the managed-cloud case.
  workspaceCreationDisabled: boolean;
  // "" = disabled, "anthropic" or "openai" = configured. Used to surface a
  // proactive hint in the persona tab before the user tries to synthesize.
  personaSynthesisBackend: string;
  // true when PERSONA_EMBEDDING_MODEL changed since embeddings were generated —
  // old vectors are incompatible and semantic memory search will return garbage.
  embeddingModelStale: boolean;
  embeddingLastRebuiltAt: string | null;
  setCdnConfig: (config: { cdnDomain: string; cdnSigned?: boolean }) => void;
  setAuthConfig: (config: {
    allowSignup: boolean;
    googleClientId?: string;
    workspaceCreationDisabled?: boolean;
    personaSynthesisBackend?: string;
    embeddingModelStale?: boolean;
    embeddingLastRebuiltAt?: string | null;
  }) => void;
  setEmbeddingModelStale: (stale: boolean) => void;
  setEmbeddingLastRebuiltAt: (ts: string) => void;
  setDaemonConfig: (config: {
    daemonServerUrl?: string;
    daemonAppUrl?: string;
  }) => void;
}

export const configStore = createStore<ConfigState>((set) => ({
  cdnDomain: "",
  cdnSigned: false,
  allowSignup: true,
  googleClientId: "",
  daemonServerUrl: "",
  daemonAppUrl: "",
  workspaceCreationDisabled: false,
  personaSynthesisBackend: "",
  embeddingModelStale: false,
  embeddingLastRebuiltAt: null,
  setCdnConfig: ({ cdnDomain, cdnSigned = false }) => set({ cdnDomain, cdnSigned }),
  setAuthConfig: ({
    allowSignup,
    googleClientId = "",
    workspaceCreationDisabled = false,
    personaSynthesisBackend = "",
    embeddingModelStale = false,
    embeddingLastRebuiltAt = null,
  }) => set({ allowSignup, googleClientId, workspaceCreationDisabled, personaSynthesisBackend, embeddingModelStale, embeddingLastRebuiltAt }),
  setEmbeddingModelStale: (stale) => set({ embeddingModelStale: stale }),
  setEmbeddingLastRebuiltAt: (ts) => set({ embeddingLastRebuiltAt: ts }),
  setDaemonConfig: ({ daemonServerUrl = "", daemonAppUrl = "" }) =>
    set({ daemonServerUrl, daemonAppUrl }),
}));

export function useConfigStore(): ConfigState;
export function useConfigStore<T>(selector: (state: ConfigState) => T): T;
export function useConfigStore<T>(selector?: (state: ConfigState) => T) {
  return useStore(configStore, selector as (state: ConfigState) => T);
}
