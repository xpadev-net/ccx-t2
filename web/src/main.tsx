import { FormEvent, StrictMode, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

type Task = {
  id: string;
  title?: string;
  status?: string;
  branch?: string;
  worker_id?: string;
  harness?: string;
  pr_url?: string;
  reason?: string;
  body?: string;
};

type TaskCreateResponse = {
  task: Task;
  orchestrator_triggered: boolean;
  trigger_error?: string;
};

type TaskDeleteResponse = {
  deleted: Task;
  worker_cleaned?: boolean;
  cleanup_error?: string;
  delete_pending?: boolean;
};

type WorkerInfo = {
  worker_id: string;
  task_id: string;
  harness?: string;
  worktree_path?: string;
  started_at?: string;
};

type ProjectInfo = {
  slug: string;
  repo_path: string;
};

type ConfigResponse = {
  project: {
    slug: string;
    repo_path: string;
    worktree_base: string;
  };
  server: {
    port: number;
  };
  runtime?: {
    tmux_session: string;
    worktree_base: string;
  };
  orchestrator: {
    harness: string;
    heartbeat_interval: string;
    timeout: string;
  };
  worker_harnesses: string[];
  harnesses: Record<string, { command: string }>;
  github: {
    owner?: string;
    repo?: string;
  };
  projects?: Record<
    string,
    {
      slug: string;
      repo_path: string;
      worktree_base: string;
      ledger_path?: string;
    }
  >;
};

type ConfigDraft = {
  projectSlug: string;
  repoPath: string;
  worktreeBase: string;
  serverPort: string;
  orchestratorHarness: string;
  heartbeatInterval: string;
  timeout: string;
  workerHarnesses: string;
  harnesses: Record<string, string>;
  githubOwner: string;
  githubRepo: string;
};

type HarnessInfo = {
  name: string;
  available: boolean;
  usage: {
    command?: string;
    note?: string;
  };
};

const statusOptions = ["unstarted", "in_progress", "blocked", "completed", "split"];
const tokenStorageKey = "ccx.webToken";

function App() {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [status, setStatus] = useState("unstarted");
  const [newTitle, setNewTitle] = useState("");
  const [newBody, setNewBody] = useState("");
  const [token, setToken] = useState(() => initialToken());
  const [tokenDraft, setTokenDraft] = useState(() => initialToken());
  const [workers, setWorkers] = useState<WorkerInfo[]>([]);
  const [selectedWorkerID, setSelectedWorkerID] = useState("");
  const [workerLog, setWorkerLog] = useState<string[]>([]);
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  const [selectedProjectSlug, setSelectedProjectSlug] = useState("");
  const [newProjectSlug, setNewProjectSlug] = useState("");
  const [newProjectRepoPath, setNewProjectRepoPath] = useState("");
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [configDraft, setConfigDraft] = useState<ConfigDraft>(() => emptyConfigDraft());
  const [settingsDirty, setSettingsDirty] = useState(false);
  const [harnesses, setHarnesses] = useState<HarnessInfo[]>([]);
  const [settingsLoading, setSettingsLoading] = useState(true);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const settingsDirtyRef = useRef(false);

  const selectedTask = useMemo(
    () => tasks.find((task) => task.id === selectedID) ?? tasks[0],
    [selectedID, tasks]
  );
  const selectedWorker = useMemo(
    () => workers.find((worker) => worker.worker_id === selectedWorkerID) ?? workers[0],
    [selectedWorkerID, workers]
  );
  const selectedWorkerTask = useMemo(
    () => tasks.find((task) => task.id === selectedWorker?.task_id),
    [selectedWorker?.task_id, tasks]
  );
  const harnessNames = useMemo(
    () => Object.keys(configDraft.harnesses).sort((a, b) => a.localeCompare(b)),
    [configDraft.harnesses]
  );

  useEffect(() => {
    void refreshAll(true, token, true);
  }, []);

  useEffect(() => {
    if (selectedProjectSlug) {
      void refreshProjectData(true);
    }
  }, [selectedProjectSlug]);

  useEffect(() => {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const tokenQuery = token ? `?token=${encodeURIComponent(token)}` : "";
    const ledgerPath = selectedProjectSlug
      ? `/ws/projects/${encodeURIComponent(selectedProjectSlug)}/ledger`
      : "/ws/ledger";
    const socket = new WebSocket(`${protocol}//${window.location.host}${ledgerPath}${tokenQuery}`);
    socket.addEventListener("message", (event) => {
      try {
        const msg = JSON.parse(String(event.data)) as { type?: string };
        if (msg.type === "ledger_changed") {
          void refreshTasks(false);
          void refreshWorkers();
        }
      } catch {
        // Ignore malformed websocket messages; the next manual refresh will recover.
      }
    });
    return () => socket.close();
  }, [selectedProjectSlug, token]);

  useEffect(() => {
    if (!selectedWorker) {
      setSelectedWorkerID("");
      setWorkerLog([]);
      return;
    }
    setSelectedWorkerID(selectedWorker.worker_id);
    setWorkerLog([]);
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const tokenQuery = token ? `?token=${encodeURIComponent(token)}` : "";
    let attempts = 0;
    let closed = false;
    let retryTimer: number | undefined;
    let socket: WebSocket | undefined;
    const connect = () => {
      if (closed) {
        return;
      }
      socket = new WebSocket(
        `${protocol}//${window.location.host}${workerLogPath(selectedProjectSlug, selectedWorker.worker_id)}${tokenQuery}`
      );
      socket.addEventListener("message", (event) => {
        try {
          const msg = JSON.parse(String(event.data)) as { type?: string; data?: string };
          if (msg.type === "line" && typeof msg.data === "string") {
            setWorkerLog((current) => [...current.slice(-299), msg.data as string]);
          } else if (msg.type === "closed") {
            setWorkerLog((current) => [...current, "[stream closed]"]);
          } else if (msg.type === "error" && msg.data) {
            setWorkerLog((current) => [...current, `[error] ${msg.data}`]);
          }
        } catch {
          setWorkerLog((current) => [...current, String(event.data)]);
        }
      });
      socket.addEventListener("error", () => {
        setWorkerLog((current) => [...current, "[stream error]"]);
      });
      socket.addEventListener("close", () => {
        if (!closed && attempts < 3) {
          attempts += 1;
          retryTimer = window.setTimeout(connect, 250 * attempts);
        }
      });
    };
    connect();
    return () => {
      closed = true;
      if (retryTimer !== undefined) {
        window.clearTimeout(retryTimer);
      }
      socket?.close();
    };
  }, [selectedProjectSlug, selectedWorker?.worker_id, token]);

  useEffect(() => {
    if (!selectedTask) {
      setSelectedID("");
      setTitle("");
      setBody("");
      setStatus("unstarted");
      return;
    }
    setSelectedID(selectedTask.id);
    setTitle(selectedTask.title ?? "");
    setBody(selectedTask.body ?? "");
    setStatus(selectedTask.status || "unstarted");
  }, [selectedTask?.id, selectedTask?.title, selectedTask?.body, selectedTask?.status]);

  async function refreshTasks(showLoading = true, authToken = token) {
    if (!selectedProjectSlug) {
      setTasks([]);
      setSelectedID("");
      setLoading(false);
      return;
    }
    if (showLoading) {
      setLoading(true);
    }
    setError("");
    try {
      const data = await api<Task[]>(tasksPath(selectedProjectSlug), {}, authToken);
      setTasks(data);
      setSelectedID((current) => current || data[0]?.id || "");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function refreshWorkers(authToken = token) {
    if (!selectedProjectSlug) {
      setWorkers([]);
      setSelectedWorkerID("");
      return;
    }
    try {
      const data = await api<WorkerInfo[]>(workersPath(selectedProjectSlug), {}, authToken);
      setWorkers(data);
      setSelectedWorkerID((current) => current || data[0]?.worker_id || "");
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  async function refreshAll(showLoading = true, authToken = token, replaceSettingsDraft = false) {
    await refreshSettings(authToken, replaceSettingsDraft);
    await refreshProjectData(showLoading, authToken);
  }

  async function refreshProjectData(showLoading = true, authToken = token) {
    await Promise.all([refreshTasks(showLoading, authToken), refreshWorkers(authToken)]);
  }

  async function refreshSettings(authToken = token, replaceDraft = false) {
    setSettingsLoading(true);
    try {
      const [configData, harnessData, projectData] = await Promise.all([
        api<ConfigResponse>("/api/config", {}, authToken),
        api<HarnessInfo[]>("/api/harnesses", {}, authToken),
        api<ProjectInfo[]>("/api/projects", {}, authToken)
      ]);
      setConfig(configData);
      setProjects(projectData);
      setSelectedProjectSlug((current) => current || projectData[0]?.slug || configData.project.slug || "");
      if (replaceDraft || !settingsDirtyRef.current) {
        setConfigDraft(configToDraft(configData));
        settingsDirtyRef.current = false;
        setSettingsDirty(false);
      }
      setHarnesses(harnessData);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSettingsLoading(false);
    }
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const response = await api<TaskCreateResponse>(
        tasksPath(selectedProjectSlug),
        {
          method: "POST",
          body: JSON.stringify({
            title: newTitle,
            body: newBody,
            status: "unstarted"
          })
        },
        token
      );
      setNewTitle("");
      setNewBody("");
      setMessage(
        response.trigger_error
          ? "Task created; orchestrator trigger failed."
          : "Task created and orchestrator notified."
      );
      await refreshAll(false);
      setSelectedID(response.task.id);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function saveTask(event: FormEvent) {
    event.preventDefault();
    if (!selectedTask) {
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    try {
      await api<Task>(
        taskPath(selectedProjectSlug, selectedTask.id),
        {
          method: "PATCH",
          body: JSON.stringify({ title, body, status })
        },
        token
      );
      setMessage("Task updated.");
      await refreshTasks(false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function deleteTask() {
    if (!selectedTask) {
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const response = await api<TaskDeleteResponse>(
        taskPath(selectedProjectSlug, selectedTask.id),
        { method: "DELETE" },
        token
      );
      if (response.cleanup_error) {
        setMessage("Delete requested; worker cleanup needs retry.");
      } else if (response.delete_pending) {
        setMessage("Delete is already in progress.");
      } else {
        setMessage("Task deleted.");
      }
      setSelectedID("");
      await refreshAll(false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function saveConfig(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const updated = await api<ConfigResponse>(
        "/api/config",
        {
          method: "PATCH",
          body: JSON.stringify(configPatchFromDraft(configDraft))
        },
        token
      );
      const harnessData = await api<HarnessInfo[]>("/api/harnesses", {}, token);
      setConfig(updated);
      setConfigDraft(configToDraft(updated));
      settingsDirtyRef.current = false;
      setSettingsDirty(false);
      setHarnesses(harnessData);
      setMessage("Settings updated.");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function addProject() {
    const slug = newProjectSlug.trim();
    const repoPath = newProjectRepoPath.trim();
    if (!slug || !repoPath) {
      setError("Project slug and repository path are required.");
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    try {
      await api<ConfigResponse>(
        "/api/config",
        {
          method: "PATCH",
          body: JSON.stringify({
            projects: {
              [slug]: {
                repo_path: repoPath,
                worktree_base: config?.runtime?.worktree_base || config?.project.worktree_base || ""
              }
            }
          })
        },
        token
      );
      setNewProjectSlug("");
      setNewProjectRepoPath("");
      setSelectedProjectSlug(slug);
      await refreshSettings(token, true);
      setMessage("Project added.");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  function updateConfigDraft(patch: Partial<ConfigDraft>) {
    markSettingsDirty();
    setConfigDraft((current) => ({ ...current, ...patch }));
  }

  function updateHarnessCommand(name: string, command: string) {
    markSettingsDirty();
    setConfigDraft((current) => ({
      ...current,
      harnesses: {
        ...current.harnesses,
        [name]: command
      }
    }));
  }

  function markSettingsDirty() {
    settingsDirtyRef.current = true;
    setSettingsDirty(true);
  }

  return (
    <main className="app-shell">
      <header className="topbar">
        <div>
          <h1>ccx-t2</h1>
          <p>Task ledger and worker orchestration</p>
        </div>
        <div className="topbar-actions">
          <label className="project-switcher">
            Project
            <select
              value={selectedProjectSlug}
              onChange={(event) => {
                setSelectedProjectSlug(event.target.value);
                setSelectedID("");
                setSelectedWorkerID("");
              }}
            >
              {projects.map((project) => (
                <option key={project.slug} value={project.slug}>
                  {project.slug}
                </option>
              ))}
              {projects.length === 0 && <option value={selectedProjectSlug}>{selectedProjectSlug || "default"}</option>}
            </select>
          </label>
          <button className="secondary" type="button" onClick={() => void refreshAll()} disabled={loading}>
            Refresh
          </button>
        </div>
      </header>

      <section className="notice-row" aria-live="polite">
        {error && <div className="notice error">{error}</div>}
        {message && <div className="notice success">{message}</div>}
      </section>

      <section className="workspace">
        <aside className="task-list" aria-label="Task ledger">
          <div className="section-heading">
            <h2>Tasks</h2>
            <span>{loading ? "Loading" : `${tasks.length} total`}</span>
          </div>
          <div className="rows">
            {tasks.map((task) => (
              <button
                className={`task-row ${task.id === selectedTask?.id ? "selected" : ""}`}
                key={task.id}
                type="button"
                onClick={() => setSelectedID(task.id)}
              >
                <span className="task-title">{task.title || task.body || task.id}</span>
                <span className={`status ${task.status || "unstarted"}`}>{task.status || "unstarted"}</span>
              </button>
            ))}
            {!loading && tasks.length === 0 && <div className="empty">No active tasks.</div>}
          </div>
        </aside>

        <section className="task-editor" aria-label="Task editor">
          <div className="section-heading">
            <h2>{selectedTask ? selectedTask.id : "No task selected"}</h2>
            {selectedTask?.worker_id && <span>{selectedTask.worker_id}</span>}
          </div>
          <form onSubmit={saveTask} className="form-grid">
            <label>
              Title
              <input value={title} onChange={(event) => setTitle(event.target.value)} disabled={!selectedTask} />
            </label>
            <label>
              Status
              <select value={status} onChange={(event) => setStatus(event.target.value)} disabled={!selectedTask}>
                {statusOptions.map((option) => (
                  <option key={option} value={option}>
                    {option}
                  </option>
                ))}
              </select>
            </label>
            <label className="wide">
              Body
              <textarea value={body} onChange={(event) => setBody(event.target.value)} disabled={!selectedTask} />
            </label>
            {selectedTask?.branch && <div className="metadata">Branch: {selectedTask.branch}</div>}
            {selectedTask?.reason && <div className="metadata">Reason: {selectedTask.reason}</div>}
            <div className="actions wide">
              <button type="submit" disabled={!selectedTask || saving}>
                Save
              </button>
              <button className="danger" type="button" onClick={() => void deleteTask()} disabled={!selectedTask || saving}>
                Delete
              </button>
            </div>
          </form>
        </section>

        <section className="task-create" aria-label="Create task">
          <div className="section-heading">
            <h2>Add Task</h2>
            <span>Orchestrator trigger</span>
          </div>
          <form onSubmit={createTask} className="form-grid">
            <label>
              Title
              <input value={newTitle} onChange={(event) => setNewTitle(event.target.value)} />
            </label>
            <label className="wide">
              Body
              <textarea value={newBody} onChange={(event) => setNewBody(event.target.value)} />
            </label>
            <div className="actions wide">
              <button type="submit" disabled={saving || (!newTitle.trim() && !newBody.trim())}>
                Create
              </button>
            </div>
          </form>
        </section>

        <section className="worker-dashboard" aria-label="Worker dashboard">
          <div className="section-heading">
            <h2>Workers</h2>
            <span>{workers.length} active</span>
          </div>
          <div className="worker-layout">
            <div className="worker-list">
              {workers.map((worker) => (
                <button
                  className={`worker-row ${worker.worker_id === selectedWorker?.worker_id ? "selected" : ""}`}
                  key={worker.worker_id}
                  type="button"
                  onClick={() => setSelectedWorkerID(worker.worker_id)}
                >
                  <span className="task-title">{worker.worker_id}</span>
                  <span>{worker.harness || "worker"}</span>
                </button>
              ))}
              {workers.length === 0 && <div className="empty">No active workers.</div>}
            </div>
            <div className="log-panel">
              <div className="log-heading">
                <span>{selectedWorker?.task_id || "No worker selected"}</span>
                {selectedWorkerTask?.branch && <span>{selectedWorkerTask.branch}</span>}
              </div>
              <pre>{workerLog.length ? workerLog.join("\n") : "Waiting for worker output..."}</pre>
            </div>
          </div>
        </section>

        <section className="settings-panel" aria-label="Settings">
          <div className="section-heading">
            <h2>Settings</h2>
            <span>{settingsDirty ? "Unsaved" : settingsLoading ? "Loading" : "Config"}</span>
          </div>
          <form onSubmit={saveConfig} className="settings-grid">
            <label>
              Project slug
              <input
                value={configDraft.projectSlug}
                onChange={(event) => updateConfigDraft({ projectSlug: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              Server port
              <input
                inputMode="numeric"
                value={configDraft.serverPort}
                onChange={(event) => updateConfigDraft({ serverPort: event.target.value })}
                disabled={!config}
              />
            </label>
            <label className="wide">
              Repository path
              <input
                value={configDraft.repoPath}
                onChange={(event) => updateConfigDraft({ repoPath: event.target.value })}
                disabled={!config}
              />
            </label>
            <label className="wide">
              Worktree base
              <input
                value={configDraft.worktreeBase}
                onChange={(event) => updateConfigDraft({ worktreeBase: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              Orchestrator harness
              <select
                value={configDraft.orchestratorHarness}
                onChange={(event) => updateConfigDraft({ orchestratorHarness: event.target.value })}
                disabled={!config}
              >
                {harnessNames.includes(configDraft.orchestratorHarness) ? null : (
                  <option value={configDraft.orchestratorHarness}>{configDraft.orchestratorHarness || "Unset"}</option>
                )}
                {harnessNames.map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Worker harnesses
              <input
                value={configDraft.workerHarnesses}
                onChange={(event) => updateConfigDraft({ workerHarnesses: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              Heartbeat interval
              <input
                value={configDraft.heartbeatInterval}
                onChange={(event) => updateConfigDraft({ heartbeatInterval: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              Orchestrator timeout
              <input
                value={configDraft.timeout}
                onChange={(event) => updateConfigDraft({ timeout: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              GitHub owner
              <input
                value={configDraft.githubOwner}
                onChange={(event) => updateConfigDraft({ githubOwner: event.target.value })}
                disabled={!config}
              />
            </label>
            <label>
              GitHub repo
              <input
                value={configDraft.githubRepo}
                onChange={(event) => updateConfigDraft({ githubRepo: event.target.value })}
                disabled={!config}
              />
            </label>

            <div className="wide harness-editor">
              <div className="subheading">
                <h3>Harness Commands</h3>
                <span>{harnessNames.length} configured</span>
              </div>
              <div className="harness-grid">
                {harnessNames.map((name) => (
                  <label key={name}>
                    {name}
                    <input
                      value={configDraft.harnesses[name] ?? ""}
                      onChange={(event) => updateHarnessCommand(name, event.target.value)}
                      disabled={!config}
                    />
                  </label>
                ))}
                {harnessNames.length === 0 && <div className="empty">No harnesses configured.</div>}
              </div>
            </div>

            <div className="wide availability-list">
              <div className="subheading">
                <h3>Availability</h3>
                <span>{harnesses.length} worker harnesses</span>
              </div>
              {harnesses.map((harness) => (
                <div className="availability-row" key={harness.name}>
                  <span>{harness.name}</span>
                  <span className={`status ${harness.available ? "completed" : "blocked"}`}>
                    {harness.available ? "available" : "unavailable"}
                  </span>
                  <span>{harness.usage.command || harness.usage.note || "No command"}</span>
                </div>
              ))}
              {!settingsLoading && harnesses.length === 0 && <div className="empty">No worker harnesses configured.</div>}
            </div>

            <div className="wide project-editor">
              <div className="subheading">
                <h3>Projects</h3>
                <span>{projects.length} configured</span>
              </div>
              <div className="project-list">
                {projects.map((project) => (
                  <div className="availability-row" key={project.slug}>
                    <span>{project.slug}</span>
                    <span>{project.repo_path}</span>
                  </div>
                ))}
                {projects.length === 0 && <div className="empty">No projects configured.</div>}
              </div>
              <div className="project-add">
                <label>
                  Slug
                  <input value={newProjectSlug} onChange={(event) => setNewProjectSlug(event.target.value)} />
                </label>
                <label>
                  Repository path
                  <input value={newProjectRepoPath} onChange={(event) => setNewProjectRepoPath(event.target.value)} />
                </label>
                <button
                  type="button"
                  onClick={() => void addProject()}
                  disabled={saving || !newProjectSlug.trim() || !newProjectRepoPath.trim()}
                >
                  Add Project
                </button>
              </div>
            </div>

            <div className="actions wide">
              <button type="submit" disabled={!config || saving}>
                Save Settings
              </button>
            </div>
          </form>
        </section>

        <section className="auth-panel" aria-label="API token">
          <div className="section-heading">
            <h2>API Token</h2>
            <span>Bearer auth</span>
          </div>
          <label>
            Token
            <input
              type="password"
              value={tokenDraft}
              onChange={(event) => setTokenDraft(event.target.value)}
            />
          </label>
          <div className="actions wide">
            <button
              type="button"
              onClick={() => {
                setToken(tokenDraft);
                storeToken(tokenDraft);
                void refreshAll(true, tokenDraft, true);
              }}
            >
              Apply
            </button>
          </div>
        </section>
      </section>
    </main>
  );
}

function emptyConfigDraft(): ConfigDraft {
  return {
    projectSlug: "",
    repoPath: "",
    worktreeBase: "",
    serverPort: "",
    orchestratorHarness: "",
    heartbeatInterval: "",
    timeout: "",
    workerHarnesses: "",
    harnesses: {},
    githubOwner: "",
    githubRepo: ""
  };
}

function configToDraft(config: ConfigResponse): ConfigDraft {
  return {
    projectSlug: config.project.slug,
    repoPath: config.project.repo_path,
    worktreeBase: config.project.worktree_base,
    serverPort: String(config.server.port),
    orchestratorHarness: config.orchestrator.harness,
    heartbeatInterval: config.orchestrator.heartbeat_interval,
    timeout: config.orchestrator.timeout,
    workerHarnesses: config.worker_harnesses.join(", "),
    harnesses: Object.fromEntries(Object.entries(config.harnesses).map(([name, harness]) => [name, harness.command])),
    githubOwner: config.github.owner ?? "",
    githubRepo: config.github.repo ?? ""
  };
}

function configPatchFromDraft(draft: ConfigDraft) {
  const trimmedPort = draft.serverPort.trim();
  if (!/^\d+$/.test(trimmedPort)) {
    throw new Error("Server port must be an integer");
  }
  const serverPort = Number(trimmedPort);
  if (serverPort < 1 || serverPort > 65535) {
    throw new Error("Server port must be between 1 and 65535");
  }
  return {
    project: {
      slug: draft.projectSlug,
      repo_path: draft.repoPath,
      worktree_base: draft.worktreeBase
    },
    server: {
      port: serverPort
    },
    orchestrator: {
      harness: draft.orchestratorHarness,
      heartbeat_interval: draft.heartbeatInterval,
      timeout: draft.timeout
    },
    worker_harnesses: draft.workerHarnesses
      .split(",")
      .map((name) => name.trim())
      .filter(Boolean),
    harnesses: Object.fromEntries(
      Object.entries(draft.harnesses).map(([name, command]) => [name, { command }])
    ),
    github: {
      owner: draft.githubOwner,
      repo: draft.githubRepo
    }
  };
}

function tasksPath(projectSlug: string) {
  return projectSlug ? `/api/projects/${encodeURIComponent(projectSlug)}/tasks` : "/api/tasks";
}

function workersPath(projectSlug: string) {
  return projectSlug ? `/api/projects/${encodeURIComponent(projectSlug)}/workers` : "/api/workers";
}

function taskPath(projectSlug: string, taskID: string) {
  return projectSlug
    ? `/api/projects/${encodeURIComponent(projectSlug)}/tasks/${encodeURIComponent(taskID)}`
    : `/api/tasks/${encodeURIComponent(taskID)}`;
}

function workerLogPath(projectSlug: string, workerID: string) {
  return projectSlug
    ? `/ws/projects/${encodeURIComponent(projectSlug)}/worker/${encodeURIComponent(workerID)}`
    : `/ws/worker/${encodeURIComponent(workerID)}`;
}

async function api<T>(path: string, init: RequestInit = {}, token = ""): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init.headers ?? {})
    }
  });
  const payload = await response.json().catch(() => undefined);
  if (!response.ok) {
    const msg = payload && typeof payload.error === "string" ? payload.error : response.statusText;
    throw new Error(msg);
  }
  return payload as T;
}

function errorMessage(err: unknown) {
  return err instanceof Error ? err.message : "Unexpected error";
}

function initialToken() {
  const params = new URLSearchParams(window.location.search);
  const token = params.get("token") || localStorage.getItem(tokenStorageKey) || "";
  if (token) {
    storeToken(token);
  }
  return token;
}

function storeToken(token: string) {
  if (token) {
    localStorage.setItem(tokenStorageKey, token);
  } else {
    localStorage.removeItem(tokenStorageKey);
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>
);
