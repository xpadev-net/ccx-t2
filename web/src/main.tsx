import { StrictMode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { createRoot } from "react-dom/client";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import {
  ApiError,
  createTerminalApiClient,
  type ProjectInfo,
  type TerminalInfo,
  type TerminalState
} from "./terminal";
import { useTerminalController } from "./terminal";
import "./styles.css";

type TerminalMap = Record<string, TerminalInfo[]>;
type TerminalBuffer = { revision: number; chunks: string[] };
type BufferMap = Record<string, TerminalBuffer>;
type ConnectionMap = Record<string, TerminalState>;

const tokenStorageKey = "ccx.webToken";
const maxBufferedChunks = 1600;
const retainedBufferedChunks = 1200;

function App() {
  const [token, setToken] = useState(() => initialToken());
  const [tokenDraft, setTokenDraft] = useState(() => initialToken());
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  const [terminalsByProject, setTerminalsByProject] = useState<TerminalMap>({});
  const [terminalLoadErrors, setTerminalLoadErrors] = useState<Record<string, string>>({});
  const [selectedProjectSlug, setSelectedProjectSlug] = useState("");
  const [selectedTerminalKey, setSelectedTerminalKey] = useState("");
  const [buffers, setBuffers] = useState<BufferMap>({});
  const [connections, setConnections] = useState<ConnectionMap>({});
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [busyAction, setBusyAction] = useState<"create" | "delete" | "">("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [retrySignals, setRetrySignals] = useState<Record<string, number>>({});
  const selectedProjectSlugRef = useRef("");
  const terminalsByProjectRef = useRef<TerminalMap>({});
  const loadGenerationRef = useRef(0);
  const terminalMutationGenerationRef = useRef(0);
  const credentialGenerationRef = useRef(0);
  const tokenRef = useRef(token);
  selectedProjectSlugRef.current = selectedProjectSlug;
  terminalsByProjectRef.current = terminalsByProject;
  tokenRef.current = token;

  const client = useMemo(() => createTerminalApiClient({ token }), [token]);

  const loadWorkspace = useCallback(
    async (signal?: AbortSignal, isRefresh = false) => {
      const loadGeneration = ++loadGenerationRef.current;
      const terminalMutationGeneration = terminalMutationGenerationRef.current;
      if (isRefresh) {
        setRefreshing(true);
      } else {
        setLoading(true);
      }
      setError("");
      setTerminalLoadErrors({});
      try {
        const nextProjects = await client.listProjects(signal);
        if (signal?.aborted || loadGeneration !== loadGenerationRef.current) {
          return;
        }
        const terminalErrors: Record<string, string> = {};
        const terminalResults = await Promise.all(
          nextProjects.map(async (project) => {
            try {
              return [project.slug, await client.listProjectTerminals(project.slug, signal)] as const;
            } catch (err) {
              if (!signal?.aborted) {
                terminalErrors[project.slug] = errorMessage(err);
              }
              return [project.slug, null] as const;
            }
          })
        );
        if (signal?.aborted || loadGeneration !== loadGenerationRef.current) {
          return;
        }
        const terminalListsAreCurrent = terminalMutationGeneration === terminalMutationGenerationRef.current;
        if (!terminalListsAreCurrent) {
          return;
        }
        const nextTerminals: TerminalMap = { ...terminalsByProjectRef.current };
        for (const [slug, terminals] of terminalResults) {
          if (terminals) {
            nextTerminals[slug] = terminals;
          }
        }
        const currentProjectSlug = selectedProjectSlugRef.current;
        const nextSelectedProjectSlug = nextProjects.some((project) => project.slug === currentProjectSlug)
          ? currentProjectSlug
          : nextProjects[0]?.slug ?? "";
        setProjects(nextProjects);
        setTerminalsByProject(nextTerminals);
        setTerminalLoadErrors(terminalErrors);
        if (Object.keys(terminalErrors).length > 0) {
          setError(Object.entries(terminalErrors).map(([slug, detail]) => `${slug}: ${detail}`).join(" · "));
        }
        setSelectedProjectSlug(nextSelectedProjectSlug);
        setSelectedTerminalKey((current) => {
          if (hasTerminalInProject(nextTerminals, nextSelectedProjectSlug, current)) {
            return current;
          }
          return firstTerminalKey(nextSelectedProjectSlug, nextTerminals);
        });
      } catch (err) {
        if (!signal?.aborted && loadGeneration === loadGenerationRef.current) {
          setError(errorMessage(err));
        }
      } finally {
        if (loadGeneration !== loadGenerationRef.current) {
          return;
        }
        if (isRefresh) {
          setRefreshing(false);
        } else {
          setLoading(false);
        }
      }
    },
    [client]
  );

  useEffect(() => {
    const controller = new AbortController();
    void loadWorkspace(controller.signal);
    return () => controller.abort();
  }, [loadWorkspace]);

  const selectedProject = useMemo(
    () => projects.find((project) => project.slug === selectedProjectSlug),
    [projects, selectedProjectSlug]
  );
  const selectedTerminals = terminalsByProject[selectedProjectSlug] ?? [];
  const selectedTerminal = selectedTerminals.find(
    (terminal) => terminalKey(selectedProjectSlug, terminal.window) === selectedTerminalKey
  );
  const selectedConnection = selectedTerminal
    ? connections[selectedTerminalKey] ?? idleState("Terminal is waiting to connect.")
    : idleState("Select a terminal to connect.");

  useEffect(() => {
    const list = terminalsByProject[selectedProjectSlug] ?? [];
    if (list.length > 0 && !list.some((terminal) => terminalKey(selectedProjectSlug, terminal.window) === selectedTerminalKey)) {
      setSelectedTerminalKey(firstTerminalKey(selectedProjectSlug, terminalsByProject));
    }
  }, [selectedProjectSlug, selectedTerminalKey, terminalsByProject]);

  const selectProject = useCallback(
    (slug: string) => {
      setSelectedProjectSlug(slug);
      setSelectedTerminalKey((current) => {
        const list = terminalsByProject[slug] ?? [];
        return list.some((terminal) => terminalKey(slug, terminal.window) === current)
          ? current
          : firstTerminalKey(slug, terminalsByProject);
      });
      setNotice("");
    },
    [terminalsByProject]
  );

  const selectTerminal = useCallback((slug: string, windowName: string) => {
    setSelectedProjectSlug(slug);
    setSelectedTerminalKey(terminalKey(slug, windowName));
    setNotice("");
  }, []);

  const refresh = useCallback(() => {
    if (loading || refreshing || busyAction) {
      return;
    }
    const controller = new AbortController();
    void loadWorkspace(controller.signal, true).finally(() => controller.abort());
  }, [busyAction, loadWorkspace, loading, refreshing]);

  const handleCreateTerminal = useCallback(async () => {
    if (!selectedProjectSlug || busyAction || loading || refreshing) {
      return;
    }
    const projectSlug = selectedProjectSlug;
    const credentialGeneration = credentialGenerationRef.current;
    const mutationToken = token;
    setBusyAction("create");
    setError("");
    setNotice("");
    try {
      const terminal = await client.createProjectTerminal(projectSlug);
      if (credentialGeneration !== credentialGenerationRef.current || mutationToken !== tokenRef.current) {
        return;
      }
      terminalMutationGenerationRef.current += 1;
      setTerminalsByProject((current) => ({
        ...current,
        [projectSlug]: [...(current[projectSlug] ?? []), terminal]
      }));
      if (selectedProjectSlugRef.current === projectSlug) {
        setSelectedTerminalKey(terminalKey(projectSlug, terminal.window));
      }
      setNotice(`Opened ${terminal.title}.`);
    } catch (err) {
      if (credentialGeneration === credentialGenerationRef.current && mutationToken === tokenRef.current) {
        setError(`Could not open a shell: ${errorMessage(err)}`);
      }
    } finally {
      if (credentialGeneration === credentialGenerationRef.current && mutationToken === tokenRef.current) {
        setBusyAction("");
      }
    }
  }, [busyAction, client, loading, refreshing, selectedProjectSlug, token]);

  const handleCloseTerminal = useCallback(async () => {
    if (!selectedTerminal?.closable || busyAction || loading || refreshing || !selectedProjectSlug) {
      return;
    }
    if (typeof window !== "undefined" && !window.confirm(`Close ${selectedTerminal.title}?`)) {
      return;
    }
    const projectSlug = selectedProjectSlug;
    const key = terminalKey(projectSlug, selectedTerminal.window);
    const windowName = selectedTerminal.window;
    const credentialGeneration = credentialGenerationRef.current;
    const mutationToken = token;
    const successor = (terminalsByProject[projectSlug] ?? []).find((terminal) => terminal.window !== windowName);
    const successorKey = successor ? terminalKey(projectSlug, successor.window) : "";
    setBusyAction("delete");
    setError("");
    setNotice("");
    try {
      await client.deleteProjectTerminal(projectSlug, windowName);
      if (credentialGeneration !== credentialGenerationRef.current || mutationToken !== tokenRef.current) {
        return;
      }
      terminalMutationGenerationRef.current += 1;
      setTerminalsByProject((current) => ({
        ...current,
        [projectSlug]: (current[projectSlug] ?? []).filter((terminal) => terminal.window !== windowName)
      }));
      setBuffers((current) => {
        const next = { ...current };
        delete next[key];
        return next;
      });
      setConnections((current) => {
        const next = { ...current };
        delete next[key];
        return next;
      });
      setRetrySignals((current) => {
        const next = { ...current };
        delete next[key];
        return next;
      });
      tabRefs.current[key] = null;
      setSelectedTerminalKey((current) => current === key ? successorKey : current);
      if (successorKey && selectedProjectSlugRef.current === projectSlug) {
        window.requestAnimationFrame(() => {
          window.requestAnimationFrame(() => tabRefs.current[successorKey]?.focus());
        });
      }
      setNotice(`${selectedTerminal.title} closed.`);
    } catch (err) {
      if (credentialGeneration === credentialGenerationRef.current && mutationToken === tokenRef.current) {
        setError(`Could not close ${selectedTerminal.title}: ${errorMessage(err)}`);
      }
    } finally {
      if (credentialGeneration === credentialGenerationRef.current && mutationToken === tokenRef.current) {
        setBusyAction("");
      }
    }
  }, [busyAction, client, loading, refreshing, selectedProjectSlug, selectedTerminal, token]);

  const handleTerminalState = useCallback((key: string, state: TerminalState, credentialGeneration: number) => {
    if (credentialGeneration !== credentialGenerationRef.current) {
      return;
    }
    setConnections((current) => ({ ...current, [key]: state }));
  }, []);

  const handleTerminalData = useCallback((key: string, data: string, credentialGeneration: number) => {
    if (credentialGeneration !== credentialGenerationRef.current) {
      return;
    }
    setBuffers((current) => {
      const previous = current[key] ?? { revision: 0, chunks: [] };
      const next = [...previous.chunks, data];
      return {
        ...current,
        [key]: {
          revision: previous.revision,
          chunks: next.length > maxBufferedChunks ? next.slice(-retainedBufferedChunks) : next
        }
      };
    });
  }, []);

  const handleTerminalSnapshot = useCallback((key: string, data: string, credentialGeneration: number) => {
    if (credentialGeneration !== credentialGenerationRef.current) {
      return;
    }
    setBuffers((current) => ({
      ...current,
      [key]: {
        revision: (current[key]?.revision ?? 0) + 1,
        chunks: data === "" ? [] : [normalizeTerminalSnapshot(data)]
      }
    }));
  }, []);

  const handleTerminalError = useCallback((title: string, err: Error, credentialGeneration: number) => {
    if (credentialGeneration !== credentialGenerationRef.current) {
      return;
    }
    setError(`${title}: ${err.message}`);
  }, []);

  const applyToken = () => {
    const nextToken = tokenDraft.trim();
    if (nextToken === token) {
      setNotice("Access token is already applied.");
      return;
    }
    storeToken(nextToken);
    credentialGenerationRef.current += 1;
    loadGenerationRef.current += 1;
    setBusyAction("");
    setProjects([]);
    setTerminalsByProject({});
    terminalsByProjectRef.current = {};
    setTerminalLoadErrors({});
    setSelectedProjectSlug("");
    selectedProjectSlugRef.current = "";
    setSelectedTerminalKey("");
    setBuffers({});
    setConnections({});
    setRetrySignals({});
    tabRefs.current = {};
    setLoading(true);
    setToken(nextToken);
    setNotice("Access token applied. Refreshing workspace…");
  };

  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});
  const renderedCredentialGeneration = credentialGenerationRef.current;
  const handleTabKeyDown = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
      if (!selectedTerminals.length || !["ArrowRight", "ArrowLeft", "Home", "End"].includes(event.key)) {
        return;
      }
      event.preventDefault();
      const nextIndex = event.key === "Home"
        ? 0
        : event.key === "End"
          ? selectedTerminals.length - 1
          : (index + (event.key === "ArrowRight" ? 1 : -1) + selectedTerminals.length) % selectedTerminals.length;
      const nextTerminal = selectedTerminals[nextIndex];
      const nextKey = terminalKey(selectedProjectSlug, nextTerminal.window);
      selectTerminal(selectedProjectSlug, nextTerminal.window);
      window.requestAnimationFrame(() => tabRefs.current[nextKey]?.focus());
    },
    [selectTerminal, selectedProjectSlug, selectedTerminals]
  );

  return (
    <div className="app-shell">
      <aside className="project-sidebar" aria-label="Projects and terminals">
        <div className="brand-lockup">
          <span className="brand-mark" aria-hidden="true">cc</span>
          <span>
            <strong>ccx-t2</strong>
            <small>WebShell</small>
          </span>
        </div>
        <div className="sidebar-heading">
          <div>
            <span className="eyebrow">Workspace</span>
            <h1>Projects</h1>
          </div>
          <button className="icon-button" type="button" onClick={refresh} disabled={loading || refreshing || Boolean(busyAction)} aria-label="Refresh projects and terminals" title="Refresh">
            {refreshing ? "…" : "↻"}
          </button>
        </div>
        <div className="project-tree">
          {loading && <div className="sidebar-empty">Loading projects…</div>}
          {!loading && projects.length === 0 && <div className="sidebar-empty">No projects are configured.</div>}
          {projects.map((project) => {
            const terminals = terminalsByProject[project.slug] ?? [];
            const terminalLoadError = terminalLoadErrors[project.slug];
            const projectSelected = project.slug === selectedProjectSlug;
            return (
              <section className={`project-group ${projectSelected ? "selected" : ""}`} key={project.slug}>
                <button className="project-button" type="button" onClick={() => selectProject(project.slug)} aria-current={projectSelected ? "page" : undefined}>
                  <span className="project-chevron" aria-hidden="true">{projectSelected ? "⌄" : "›"}</span>
                  <span className="project-copy">
                    <strong>{project.slug}</strong>
                    <small title={project.repo_path}>{project.repo_path}</small>
                  </span>
                  <span className="count-badge">{terminals.length}</span>
                </button>
                <div className="terminal-tree" role="group" aria-label={`${project.slug} terminals`}>
                    {terminals.map((terminal) => {
                      const key = terminalKey(project.slug, terminal.window);
                      const isSelected = key === selectedTerminalKey;
                      return (
                        <button className={`terminal-entry ${isSelected ? "selected" : ""}`} key={terminal.window} type="button" onClick={() => selectTerminal(project.slug, terminal.window)} aria-current={isSelected ? "true" : undefined}>
                          <span className={`terminal-glyph ${terminal.kind}`} aria-hidden="true">{terminal.kind === "shell" ? "$" : "•"}</span>
                          <span className="terminal-copy"><strong>{terminal.title}</strong><small>{terminal.kind}{terminal.closable ? " · closable" : " · protected"}</small></span>
                          <span className={`availability-dot ${terminal.available ? "available" : "missing"}`} title={terminal.available ? "Available" : "Unavailable"} aria-label={terminal.available ? "Available" : "Unavailable"} />
                        </button>
                      );
                    })}
                    {terminalLoadError && <div className="terminal-empty error-text">Terminal list unavailable. {terminalLoadError}</div>}
                    {!terminalLoadError && terminals.length === 0 && <div className="terminal-empty">No shell windows yet.</div>}
                </div>
              </section>
            );
          })}
        </div>
        <div className="sidebar-footer">
          <label className="token-field">
            <span>Access token</span>
            <input type="password" value={tokenDraft} onChange={(event) => setTokenDraft(event.target.value)} placeholder="Optional bearer token" aria-label="Access token" />
          </label>
          <button className="sidebar-token-button" type="button" onClick={applyToken} disabled={loading || refreshing || Boolean(busyAction)}>Apply token</button>
        </div>
      </aside>

      <main className="workspace" aria-label="Terminal workspace">
        <header className="workspace-header">
          <div className="workspace-title">
            <span className="eyebrow">Project workspace</span>
            <h2>{selectedProject?.slug ?? "No project selected"}</h2>
            <p>{selectedProject?.repo_path ?? "Choose a project from the sidebar to open a terminal."}</p>
          </div>
          <div className="workspace-actions">
            <button className="toolbar-button" type="button" onClick={refresh} disabled={loading || refreshing || Boolean(busyAction)} aria-label="Refresh terminal list">{refreshing ? "Refreshing…" : "Refresh"}</button>
            <button className="primary-button" type="button" onClick={() => void handleCreateTerminal()} disabled={!selectedProjectSlug || Boolean(busyAction) || loading || refreshing} aria-label="Open a new shell">＋ New shell</button>
          </div>
        </header>

        {(error || notice) && (
          <div className={`notice-bar ${error ? "error" : "success"}`} role={error ? "alert" : "status"}>
            <span>{error || notice}</span>
            <button className="notice-dismiss" type="button" onClick={() => { setError(""); setNotice(""); }} aria-label="Dismiss notification">×</button>
          </div>
        )}

        <div className="tab-strip" role="tablist" aria-label="Project terminal tabs" aria-orientation="horizontal">
          <div className="tab-strip-label"><span className="live-pip" aria-hidden="true" /> terminals</div>
          <div className="tabs-scroll">
            {selectedTerminals.map((terminal, index) => {
              const key = terminalKey(selectedProjectSlug, terminal.window);
              const selected = key === selectedTerminalKey;
              const state = connections[key] ?? idleState("Terminal is idle.");
              const tabId = terminalDomId("terminal-tab", key);
              const panelId = terminalDomId("terminal-panel", key);
              return (
                <button className={`terminal-tab ${selected ? "selected" : ""}`} type="button" role="tab" id={tabId} aria-controls={panelId} aria-selected={selected} tabIndex={selected ? 0 : -1} key={terminal.window} ref={(element) => { tabRefs.current[key] = element; }} onClick={() => selectTerminal(selectedProjectSlug, terminal.window)} onKeyDown={(event) => handleTabKeyDown(event, index)}>
                  <span className={`tab-status ${state.phase}`} aria-hidden="true" />
                  <span className="tab-title">{terminal.title}</span>
                  {!terminal.closable && <span className="protected-mark" title="Protected terminal" aria-label="Protected terminal">◆</span>}
                </button>
              );
            })}
            {selectedTerminals.length === 0 && <span className="tabs-empty">No tabs</span>}
          </div>
        </div>

        <section className="terminal-stage" id={selectedTerminal ? terminalDomId("terminal-panel", selectedTerminalKey) : "terminal-panel-empty"} role="tabpanel" aria-labelledby={selectedTerminal ? terminalDomId("terminal-tab", selectedTerminalKey) : undefined} tabIndex={0} aria-label="Interactive terminal">
          {selectedTerminal ? (
            <TerminalSurface
              key={selectedTerminalKey}
              projectSlug={selectedProjectSlug}
              terminal={selectedTerminal}
              token={token}
              buffer={buffers[selectedTerminalKey] ?? { revision: 0, chunks: [] }}
              retrySignal={retrySignals[selectedTerminalKey] ?? 0}
              onSnapshot={(data) => handleTerminalSnapshot(selectedTerminalKey, data, renderedCredentialGeneration)}
              onData={(data) => handleTerminalData(selectedTerminalKey, data, renderedCredentialGeneration)}
              onStateChange={(state) => handleTerminalState(selectedTerminalKey, state, renderedCredentialGeneration)}
              onError={(err) => handleTerminalError(selectedTerminal.title, err, renderedCredentialGeneration)}
            />
          ) : (
            <WorkspaceEmptyState loading={loading} hasProjects={projects.length > 0} onCreate={() => void handleCreateTerminal()} />
          )}
        </section>

        <footer className="status-bar">
          <div className="status-context"><span className={`state-dot ${selectedConnection.phase}`} aria-hidden="true" /> <span>{selectedTerminal?.title ?? "No terminal"}</span><span className="status-separator">/</span><span>{selectedProjectSlug || "No project"}</span></div>
          <div className="status-actions">
            <span className={`connection-text ${selectedConnection.phase}`}>{terminalStateLabel(selectedConnection)}</span>
            {selectedTerminal?.closable && <button className="status-button danger-text" type="button" onClick={() => void handleCloseTerminal()} disabled={Boolean(busyAction) || loading || refreshing} aria-label={`Close ${selectedTerminal.title}`}>Close</button>}
            <button className="status-button" type="button" onClick={() => {
              if (selectedTerminal) {
                setRetrySignals((current) => ({ ...current, [selectedTerminalKey]: (current[selectedTerminalKey] ?? 0) + 1 }));
              }
            }} disabled={!selectedTerminal || Boolean(busyAction)} aria-label="Reconnect selected terminal">Reconnect</button>
          </div>
        </footer>
      </main>
    </div>
  );
}

function TerminalSurface({
  projectSlug,
  terminal,
  token,
  buffer,
  retrySignal,
  onSnapshot,
  onData,
  onStateChange,
  onError
}: {
  projectSlug: string;
  terminal: TerminalInfo;
  token: string;
  buffer: TerminalBuffer;
  retrySignal: number;
  onSnapshot: (data: string) => void;
  onData: (data: string) => void;
  onStateChange: (state: TerminalState) => void;
  onError: (error: Error) => void;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const writtenRef = useRef(0);
  const revisionRef = useRef(buffer.revision);
  const sendInputRef = useRef<(data: string) => boolean>(() => false);
  const resizeRef = useRef<(size: { cols: number; rows: number }) => boolean>(() => false);
  const retryRef = useRef<() => void>(() => undefined);
  const initialRetrySignalRef = useRef(retrySignal);
  const terminalKeyValue = terminalKey(projectSlug, terminal.window);

  const handleSnapshot = useCallback((data: string) => onSnapshot(data), [onSnapshot]);
  const handleData = useCallback((data: string) => onData(data), [onData]);
  const handleState = useCallback((state: TerminalState) => onStateChange(state), [onStateChange]);
  const handleError = useCallback((error: Error) => onError(error), [onError]);
  const { state, retry, sendInput, resize } = useTerminalController({
    projectSlug,
    windowName: terminal.window,
    token,
    onSnapshot: handleSnapshot,
    onData: handleData,
    onStateChange: handleState,
    onError: handleError
  });

  sendInputRef.current = sendInput;
  resizeRef.current = resize;
  retryRef.current = retry;

  useEffect(() => {
    if (retrySignal > initialRetrySignalRef.current) {
      retryRef.current();
    }
    initialRetrySignalRef.current = retrySignal;
  }, [retrySignal]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) {
      return;
    }
    const xterm = new Terminal({
      allowProposedApi: true,
      convertEol: false,
      cursorBlink: true,
      cursorStyle: "bar",
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
      fontSize: 14,
      scrollback: 10000,
      theme: {
        background: "#111418",
        foreground: "#d7dde5",
        cursor: "#79d2b0",
        selectionBackground: "#2f5360",
        black: "#1b2027",
        red: "#e27d86",
        green: "#83d29a",
        yellow: "#e7c27d",
        blue: "#82a9e6",
        magenta: "#c49ad6",
        cyan: "#70c5c6",
        white: "#d7dde5",
        brightBlack: "#6c7784",
        brightWhite: "#ffffff"
      }
    });
    const fit = new FitAddon();
    xterm.loadAddon(fit);
    xterm.open(container);
    terminalRef.current = xterm;
    const dataDisposable = xterm.onData((data) => { sendInputRef.current(data); });
    let frame = 0;
    const fitTerminal = () => {
      window.cancelAnimationFrame(frame);
      frame = window.requestAnimationFrame(() => {
        try {
          fit.fit();
          if (xterm.cols > 0 && xterm.rows > 0) {
            resizeRef.current({ cols: xterm.cols, rows: xterm.rows });
          }
        } catch {
          // Layout can report zero dimensions while the stage is settling.
        }
      });
    };
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(fitTerminal);
    observer?.observe(container);
    window.addEventListener("resize", fitTerminal);
    fitTerminal();
    xterm.focus();
    return () => {
      window.cancelAnimationFrame(frame);
      observer?.disconnect();
      window.removeEventListener("resize", fitTerminal);
      dataDisposable.dispose();
      xterm.dispose();
      terminalRef.current = null;
      writtenRef.current = 0;
      revisionRef.current = buffer.revision;
    };
  }, []);

  useEffect(() => {
    const xterm = terminalRef.current;
    if (!xterm) {
      return;
    }
    if (revisionRef.current !== buffer.revision || writtenRef.current > buffer.chunks.length) {
      // Queue RIS with prior writes so an older asynchronous xterm write cannot
      // land after a synchronous reset and resurrect stale pre-snapshot output.
      xterm.write("\x1bc");
      writtenRef.current = 0;
      revisionRef.current = buffer.revision;
    }
    for (let index = writtenRef.current; index < buffer.chunks.length; index += 1) {
      xterm.write(buffer.chunks[index]);
    }
    writtenRef.current = buffer.chunks.length;
  }, [buffer]);

  const placeholder = state.phase === "open" && buffer.chunks.length > 0 ? "" : terminalStateDetail(state, terminal.available);
  return (
    <div className="terminal-view" onMouseDown={() => terminalRef.current?.focus()}>
      <div className="terminal-view-header">
        <div><span className="terminal-view-title">{terminal.title}</span><span className="terminal-view-kind">{terminal.kind}{terminal.closable ? "" : " · protected"}</span></div>
        <span className={`connection-badge ${state.phase}`} role="status">{terminalStateLabel(state)}</span>
      </div>
      <div className="terminal-canvas" ref={containerRef} aria-label={`${terminal.title} terminal`} />
      {placeholder && <div className="terminal-placeholder" role="status">{placeholder}</div>}
      <div className="terminal-hint">Click to focus · terminal input is sent to {terminal.title}</div>
      <span className="sr-only">Terminal identity: {terminalKeyValue}</span>
    </div>
  );
}

function WorkspaceEmptyState({ loading, hasProjects, onCreate }: { loading: boolean; hasProjects: boolean; onCreate: () => void }) {
  return (
    <div className="workspace-empty">
      <div className="empty-icon" aria-hidden="true">⌘</div>
      <h3>{loading ? "Loading your workspace" : hasProjects ? "No terminal selected" : "Your workspace is empty"}</h3>
      <p>{loading ? "Finding projects and shell windows…" : hasProjects ? "Choose a terminal from the sidebar or open a new shell." : "Configure a project on the server to begin."}</p>
      {hasProjects && <button className="primary-button" type="button" onClick={onCreate}>＋ Open shell</button>}
    </div>
  );
}

function terminalKey(projectSlug: string, windowName: string) {
  return `${projectSlug}\u0000${windowName}`;
}

function normalizeTerminalSnapshot(data: string) {
  // tmux capture-pane separates rows with LF while the interactive xterm keeps
  // convertEol disabled so live PTY bytes remain exact. Snapshot rows therefore
  // need an explicit carriage return without altering live stream bytes.
  return data.replace(/\r?\n/g, "\r\n");
}

function terminalDomId(prefix: string, key: string) {
  return `${prefix}-${encodeURIComponent(key).replace(/%/g, "_")}`;
}

function hasTerminalInProject(terminalsByProject: TerminalMap, projectSlug: string, key: string) {
  return (terminalsByProject[projectSlug] ?? []).some((terminal) => terminalKey(projectSlug, terminal.window) === key);
}

function firstTerminalKey(projectSlug: string | undefined, terminalsByProject: TerminalMap) {
  if (!projectSlug) {
    return "";
  }
  const terminal = terminalsByProject[projectSlug]?.[0];
  return terminal ? terminalKey(projectSlug, terminal.window) : "";
}

function idleState(detail: string): TerminalState {
  return { phase: "idle", detail };
}

function terminalStateLabel(state: TerminalState) {
  switch (state.phase) {
    case "idle": return "Idle";
    case "connecting": return "Connecting";
    case "open": return "Live";
    case "retrying": return `Retrying ${state.attempt}`;
    case "missing": return "Missing";
    case "unauthorized": return "Unauthorized";
    case "failed": return "Disconnected";
  }
}

function terminalStateDetail(state: TerminalState, available: boolean) {
  if (!available) {
    return "This terminal is unavailable. Refresh the workspace or reconnect when it returns.";
  }
  if (state.phase === "retrying") {
    return `${state.detail} Retrying in ${formatDelay(state.retryInMs)}.`;
  }
  if (state.phase === "open") {
    return "Connected. Waiting for terminal output…";
  }
  return state.detail || terminalStateLabel(state);
}

function formatDelay(delay: number) {
  return delay < 1000 ? `${delay}ms` : `${(delay / 1000).toFixed(1)}s`;
}

function errorMessage(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Unauthorized. Apply a valid access token.";
    if (error.status === 404) return "Project or terminal was not found.";
    return error.message || `Request failed (${error.status}).`;
  }
  return error instanceof Error ? error.message : "Unexpected request failure.";
}

function initialToken() {
  if (typeof window === "undefined") {
    return "";
  }
  const token = new URLSearchParams(window.location.search).get("token") || window.localStorage.getItem(tokenStorageKey) || "";
  if (token) {
    storeToken(token);
  }
  return token;
}

function storeToken(token: string) {
  if (typeof window === "undefined") {
    return;
  }
  if (token) {
    window.localStorage.setItem(tokenStorageKey, token);
  } else {
    window.localStorage.removeItem(tokenStorageKey);
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>
);
