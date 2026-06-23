import { StrictMode, useEffect, useMemo, useRef, useState } from "react";
import type { Dispatch, FormEvent, SetStateAction } from "react";
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

type WorkerFollowupResponse = {
  sent: boolean;
  task_id: string;
  worker_id: string;
  session: string;
  window: string;
};

type OrchestratorInputResponse = {
  sent: boolean;
  session: string;
  window: string;
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
  session?: string;
};

type ConfigResponse = {
  project: {
    slug: string;
    repo_path: string;
    worktree_base: string;
  };
  server: {
    host: string;
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
  serverHost: string;
  serverPort: string;
  orchestratorHarness: string;
  heartbeatInterval: string;
  timeout: string;
  workerHarnesses: string;
  harnesses: Record<string, string>;
  githubOwner: string;
  githubRepo: string;
};

type TaskEditorDirty = {
  title: boolean;
  body: boolean;
  status: boolean;
};

type ConnectionPhase =
  | "idle"
  | "connecting"
  | "open"
  | "retrying"
  | "unauthorized"
  | "forbidden"
  | "blocked"
  | "missing"
  | "failed";
type FailureConnectionPhase = "unauthorized" | "forbidden" | "blocked" | "missing" | "failed";

type ConnectionState =
  | {
      phase: Exclude<ConnectionPhase, "retrying">;
      detail?: string;
    }
  | {
      phase: "retrying";
      detail?: string;
      attempt: number;
      maxAttempts: number;
      retryInMs: number;
    };

type HarnessInfo = {
  name: string;
  available: boolean;
  usage: {
    command?: string;
    note?: string;
  };
};

type WebSocketDiagnosis = {
  phase: FailureConnectionPhase;
  detail: string;
  retryable: boolean;
};

type ReconnectingWebSocketOptions = {
  path: string;
  token: string;
  setState: (state: ConnectionState) => void;
  onMessage: (event: MessageEvent) => void;
  onSocketError?: () => void;
};

type LogSetter = Dispatch<SetStateAction<string[]>>;

const statusOptions = ["unstarted", "in_progress", "blocked", "completed", "split"];
const tokenStorageKey = "ccx.webToken";
const reconnectDelaysMs = [250, 500, 1000, 2000, 4000];
const stableOpenResetDelayMs = 10000;

function App() {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [status, setStatus] = useState("unstarted");
  const [newRequest, setNewRequest] = useState("");
  const [token, setToken] = useState(() => initialToken());
  const [tokenDraft, setTokenDraft] = useState(() => initialToken());
  const [workers, setWorkers] = useState<WorkerInfo[]>([]);
  const [selectedWorkerID, setSelectedWorkerID] = useState("");
  const [orchestratorLog, setOrchestratorLog] = useState<string[]>([]);
  const [workerLog, setWorkerLog] = useState<string[]>([]);
  const [ledgerConnection, setLedgerConnection] = useState<ConnectionState>(() => idleConnection("Ledger stream is idle."));
  const [orchestratorConnection, setOrchestratorConnection] = useState<ConnectionState>(() =>
    idleConnection("Select a project to open the orchestrator log.")
  );
  const [workerConnection, setWorkerConnection] = useState<ConnectionState>(() =>
    idleConnection("Select a worker to open its log stream.")
  );
  const [orchestratorInput, setOrchestratorInput] = useState("");
  const [orchestratorInputSending, setOrchestratorInputSending] = useState(false);
  const [followupMessage, setFollowupMessage] = useState("");
  const [followupSending, setFollowupSending] = useState(false);
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
  const [warning, setWarning] = useState("");
  const [error, setError] = useState("");
  const settingsDirtyRef = useRef(false);
  const selectedProjectSlugRef = useRef("");
  const tokenRef = useRef(token);
  const taskEditorDirtyRef = useRef<TaskEditorDirty>(emptyTaskEditorDirty());
  const taskEditorTaskIDRef = useRef("");

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
  const selectedProject = useMemo(
    () => projects.find((project) => project.slug === selectedProjectSlug),
    [projects, selectedProjectSlug]
  );
  const harnessNames = useMemo(
    () => Object.keys(configDraft.harnesses).sort((a, b) => a.localeCompare(b)),
    [configDraft.harnesses]
  );
  const tmuxSession = selectedProject?.session || config?.runtime?.tmux_session || selectedProjectSlug || "tmux";
  const orchestratorWindow = selectedProjectSlug ? `${selectedProjectSlug}-orchestrator` : "orchestrator";

  useEffect(() => {
    void refreshAll(true, token, true);
  }, []);

  useEffect(() => {
    if (selectedProjectSlug) {
      void refreshProjectData(true);
    }
  }, [selectedProjectSlug]);

  useEffect(() => {
    tokenRef.current = token;
  }, [token]);

  useEffect(() => {
    return openReconnectingWebSocket({
      path: ledgerWSPath(selectedProjectSlug),
      token,
      setState: setLedgerConnection,
      onMessage: (event) => {
        try {
          const msg = JSON.parse(String(event.data)) as { type?: string };
          if (msg.type === "ledger_changed") {
            void refreshProjectData(false);
          }
        } catch {
          // Ignore malformed websocket messages; the next manual refresh will recover.
        }
      }
    });
  }, [selectedProjectSlug, token]);

  useEffect(() => {
    if (!selectedWorker) {
      setSelectedWorkerID("");
      setWorkerLog([]);
      setWorkerConnection(idleConnection("Select a worker to open its log stream."));
      setFollowupMessage("");
      return;
    }
    setSelectedWorkerID(selectedWorker.worker_id);
    setWorkerLog([]);
    setFollowupMessage("");
    return openReconnectingWebSocket({
      path: workerLogPath(selectedProjectSlug, selectedWorker.worker_id),
      token,
      setState: setWorkerConnection,
      onMessage: (event) => appendLogEvent(event, setWorkerLog),
      onSocketError: () => appendLogLine(setWorkerLog, "[stream error]", false)
    });
  }, [selectedProjectSlug, selectedWorker?.worker_id, token]);

  useEffect(() => {
    if (!selectedProjectSlug) {
      setOrchestratorLog([]);
      setOrchestratorConnection(idleConnection("Select a project to open the orchestrator log."));
      setOrchestratorInput("");
      return;
    }
    setOrchestratorLog([]);
    return openReconnectingWebSocket({
      path: orchestratorLogPath(selectedProjectSlug),
      token,
      setState: setOrchestratorConnection,
      onMessage: (event) => appendLogEvent(event, setOrchestratorLog),
      onSocketError: () => appendLogLine(setOrchestratorLog, "[stream error]", false)
    });
  }, [selectedProjectSlug, token]);

  useEffect(() => {
    if (!selectedTask) {
      setSelectedID("");
      setTitle("");
      setBody("");
      setStatus("unstarted");
      taskEditorTaskIDRef.current = "";
      clearTaskEditorDirty();
      return;
    }
    const taskChanged = taskEditorTaskIDRef.current !== selectedTask.id;
    setSelectedID(selectedTask.id);
    if (taskChanged) {
      taskEditorTaskIDRef.current = selectedTask.id;
      setTitle(selectedTask.title ?? "");
      setBody(selectedTask.body ?? "");
      setStatus(selectedTask.status || "unstarted");
      clearTaskEditorDirty();
      return;
    }
    const dirty = taskEditorDirtyRef.current;
    if (!dirty.title) {
      setTitle(selectedTask.title ?? "");
    }
    if (!dirty.body) {
      setBody(selectedTask.body ?? "");
    }
    if (!dirty.status) {
      setStatus(selectedTask.status || "unstarted");
    }
  }, [selectedTask?.id, selectedTask?.title, selectedTask?.body, selectedTask?.status]);

  async function refreshTasks(
    showLoading = true,
    authToken = token,
    projectSlug = selectedProjectSlug,
    rethrowErrors = false
  ) {
    if (!projectSlug) {
      if (isCurrentRefresh(projectSlug, authToken)) {
        setTasks([]);
        setSelectedID("");
        setLoading(false);
      }
      return;
    }
    const currentRefresh = isCurrentRefresh(projectSlug, authToken);
    if (showLoading && currentRefresh) {
      setLoading(true);
    }
    if (currentRefresh) {
      setError("");
    }
    try {
      const data = await api<Task[]>(tasksPath(projectSlug), {}, authToken);
      if (!isCurrentRefresh(projectSlug, authToken)) {
        return;
      }
      setTasks(data);
      setSelectedID((current) => (data.some((task) => task.id === current) ? current : data[0]?.id || ""));
    } catch (err) {
      if (!isCurrentRefresh(projectSlug, authToken)) {
        return;
      }
      setError(errorMessage(err));
      if (rethrowErrors) {
        throw err;
      }
    } finally {
      if (isCurrentRefresh(projectSlug, authToken)) {
        setLoading(false);
      }
    }
  }

  async function refreshWorkers(authToken = token, projectSlug = selectedProjectSlug, rethrowErrors = false) {
    if (!projectSlug) {
      if (isCurrentRefresh(projectSlug, authToken)) {
        setWorkers([]);
        setSelectedWorkerID("");
      }
      return;
    }
    try {
      const data = await api<WorkerInfo[]>(workersPath(projectSlug), {}, authToken);
      if (!isCurrentRefresh(projectSlug, authToken)) {
        return;
      }
      setWorkers(data);
      setSelectedWorkerID((current) =>
        data.some((worker) => worker.worker_id === current) ? current : data[0]?.worker_id || ""
      );
    } catch (err) {
      if (!isCurrentRefresh(projectSlug, authToken)) {
        return;
      }
      setError(errorMessage(err));
      if (rethrowErrors) {
        throw err;
      }
    }
  }

  async function refreshAll(showLoading = true, authToken = token, replaceSettingsDraft = false) {
    const projectSlug = await refreshSettings(authToken, replaceSettingsDraft);
    await refreshProjectData(showLoading, authToken, projectSlug);
  }

  async function refreshProjectData(
    showLoading = true,
    authToken = token,
    projectSlug = selectedProjectSlug,
    rethrowErrors = false
  ) {
    await Promise.all([
      refreshTasks(showLoading, authToken, projectSlug, rethrowErrors),
      refreshWorkers(authToken, projectSlug, rethrowErrors)
    ]);
  }

  async function refreshSettings(
    authToken = token,
    replaceDraft = false,
    preferredProjectSlug = "",
    rethrowErrors = false
  ) {
    setSettingsLoading(true);
    try {
      const [configData, harnessData, projectData] = await Promise.all([
        api<ConfigResponse>("/api/config", {}, authToken),
        api<HarnessInfo[]>("/api/harnesses", {}, authToken),
        api<ProjectInfo[]>("/api/projects", {}, authToken)
      ]);
      if (authToken !== tokenRef.current) {
        return selectedProjectSlugRef.current;
      }
      setConfig(configData);
      setProjects(projectData);
      const normalizedProjectSlug = normalizedProjectSelection(
        projectData,
        selectedProjectSlugRef.current,
        preferredProjectSlug
      );
      setProjectSlug(normalizedProjectSlug);
      if (replaceDraft || !settingsDirtyRef.current) {
        setConfigDraft(configToDraft(configData));
        settingsDirtyRef.current = false;
        setSettingsDirty(false);
      }
      setHarnesses(harnessData);
      return normalizedProjectSlug;
    } catch (err) {
      if (authToken !== tokenRef.current) {
        return selectedProjectSlugRef.current;
      }
      setError(errorMessage(err));
      if (rethrowErrors) {
        throw err;
      }
      return selectedProjectSlugRef.current;
    } finally {
      if (authToken === tokenRef.current) {
        setSettingsLoading(false);
      }
    }
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    const request = newRequest.trim();
    if (!request) {
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      const response = await api<TaskCreateResponse>(
        tasksPath(selectedProjectSlug),
        {
          method: "POST",
          body: JSON.stringify({
            request
          })
        },
        token
      );
      setNewRequest("");
      if (response.trigger_error) {
        setWarning(`Task created; orchestrator trigger failed: ${response.trigger_error}`);
      } else if (!response.orchestrator_triggered) {
        setWarning("Task created; orchestrator was not triggered.");
      } else {
        setMessage("Task created and orchestrator notified.");
      }
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
    setWarning("");
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
      clearTaskEditorDirty();
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
    setWarning("");
    try {
      const response = await api<TaskDeleteResponse>(
        taskPath(selectedProjectSlug, selectedTask.id),
        { method: "DELETE" },
        token
      );
      if (response.cleanup_error) {
        setWarning(`Delete requested; worker cleanup needs retry: ${response.cleanup_error}`);
      } else if (response.delete_pending) {
        setWarning("Delete is already in progress.");
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

  async function sendWorkerFollowup(event: FormEvent) {
    event.preventDefault();
    if (!selectedWorker || !followupMessage.trim()) {
      return;
    }
    setFollowupSending(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      const response = await api<WorkerFollowupResponse>(
        workerFollowupPath(selectedProjectSlug, selectedWorker.worker_id),
        {
          method: "POST",
          body: JSON.stringify({ message: followupMessage })
        },
        token
      );
      setFollowupMessage("");
      setMessage(`Followup sent to ${response.window}.`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setFollowupSending(false);
    }
  }

  async function sendOrchestratorInput(event: FormEvent) {
    event.preventDefault();
    if (!selectedProjectSlug || !orchestratorInput.trim()) {
      return;
    }
    setOrchestratorInputSending(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      const response = await api<OrchestratorInputResponse>(
        orchestratorInputPath(selectedProjectSlug),
        {
          method: "POST",
          body: JSON.stringify({ message: orchestratorInput })
        },
        token
      );
      setOrchestratorInput("");
      setMessage(`Input sent to ${response.window}.`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setOrchestratorInputSending(false);
    }
  }

  async function saveConfig(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      const updated = await api<ConfigResponse>(
        "/api/config",
        {
          method: "PATCH",
          body: JSON.stringify(configPatchFromDraft(configDraft))
        },
        token
      );
      settingsDirtyRef.current = false;
      setSettingsDirty(false);
      const projectSlug = await refreshSettings(token, true, updated.project.slug, true);
      await refreshProjectData(false, token, projectSlug, true);
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
      setWarning("");
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      await api<ProjectInfo>(
        "/api/projects",
        {
          method: "POST",
          body: JSON.stringify({
            slug,
            repo_path: repoPath,
            worktree_base: config?.runtime?.worktree_base || config?.project.worktree_base || ""
          })
        },
        token
      );
      setNewProjectSlug("");
      setNewProjectRepoPath("");
      setProjectSlug(slug);
      await refreshSettings(token, true);
      setMessage("Project added.");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function deleteProject(slug: string) {
    if (!slug || !window.confirm(`Delete project "${slug}" from this server?`)) {
      return;
    }
    setSaving(true);
    setError("");
    setMessage("");
    setWarning("");
    try {
      await api<{ status: string }>(`/api/projects/${encodeURIComponent(slug)}`, { method: "DELETE" }, token);
      setTasks([]);
      setWorkers([]);
      setSelectedID("");
      setSelectedWorkerID("");
      setProjectSlug(selectedProjectSlugRef.current === slug ? "" : selectedProjectSlugRef.current);
      await refreshSettings(token, true);
      setMessage("Project deleted.");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  function selectProject(slug: string) {
    setProjectSlug(slug);
    setTasks([]);
    setWorkers([]);
    setSelectedID("");
    setSelectedWorkerID("");
    setWorkerLog([]);
    setOrchestratorLog([]);
    setOrchestratorInput("");
    setFollowupMessage("");
  }

  function updateConfigDraft(patch: Partial<ConfigDraft>) {
    markSettingsDirty();
    setConfigDraft((current) => ({ ...current, ...patch }));
  }

  function setProjectSlug(slug: string) {
    selectedProjectSlugRef.current = slug;
    setSelectedProjectSlug(slug);
  }

  function isCurrentRefresh(projectSlug: string, authToken: string) {
    return projectSlug === selectedProjectSlugRef.current && authToken === tokenRef.current;
  }

  function updateTaskTitle(value: string) {
    setTitle(value);
    markTaskEditorDirty("title");
  }

  function updateTaskBody(value: string) {
    setBody(value);
    markTaskEditorDirty("body");
  }

  function updateTaskStatus(value: string) {
    setStatus(value);
    markTaskEditorDirty("status");
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

  function markTaskEditorDirty(field: keyof TaskEditorDirty) {
    taskEditorDirtyRef.current = {
      ...taskEditorDirtyRef.current,
      [field]: true
    };
  }

  function clearTaskEditorDirty() {
    taskEditorDirtyRef.current = emptyTaskEditorDirty();
  }

  return (
    <main className="app-shell">
      <header className="topbar">
        <div>
          <h1>ccx-t2</h1>
          <p>Task ledger and worker orchestration</p>
        </div>
        <div className="topbar-actions">
          <button className="secondary" type="button" onClick={() => void refreshAll()} disabled={loading}>
            Refresh
          </button>
        </div>
      </header>

      <section className="notice-row" aria-live="polite">
        {error && <div className="notice error">{error}</div>}
        {warning && <div className="notice warning">{warning}</div>}
        {message && <div className="notice success">{message}</div>}
      </section>

      <section className="workspace">
        <aside className="project-sidebar" aria-label="Projects">
          <div className="section-heading">
            <h2>Projects</h2>
            <span>{projects.length} total</span>
          </div>
          <div className="project-overview">
            <div>
              <span>Current</span>
              <strong>{selectedProject?.slug || "None"}</strong>
            </div>
            <div>
              <span>Tasks</span>
              <strong>{selectedProject ? tasks.length : 0}</strong>
            </div>
            <div>
              <span>Workers</span>
              <strong>{selectedProject ? workers.length : 0}</strong>
            </div>
          </div>
          <div className="project-list">
            {projects.map((project) => {
              const isSelected = project.slug === selectedProjectSlug;
              return (
                <button
                  aria-current={isSelected ? "true" : undefined}
                  className={`project-row ${isSelected ? "selected" : ""}`}
                  key={project.slug}
                  type="button"
                  onClick={() => selectProject(project.slug)}
                >
                  <span>{project.slug}</span>
                  <small>{project.repo_path}</small>
                </button>
              );
            })}
            {projects.length === 0 && <div className="empty">No projects configured.</div>}
          </div>
          <div className="sidebar-project-actions">
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
            <button
              className="danger"
              type="button"
              onClick={() => void deleteProject(selectedProjectSlug)}
              disabled={saving || !selectedProjectSlug}
            >
              Delete Selected
            </button>
          </div>
        </aside>

        <aside className="task-list" aria-label="Task ledger">
          <div className="section-heading">
            <h2>Tasks</h2>
            <div className="heading-status">
              <span>{loading ? "Loading" : `${tasks.length} total`}</span>
              <ConnectionBadge label="Ledger" state={ledgerConnection} />
            </div>
          </div>
          <div className="rows">
            {tasks.map((task) => {
              const isSelected = task.id === selectedTask?.id;
              return (
                <button
                  aria-current={isSelected ? "true" : undefined}
                  className={`task-row ${isSelected ? "selected" : ""}`}
                  key={task.id}
                  type="button"
                  onClick={() => setSelectedID(task.id)}
                >
                  <span className="task-title">{task.title || task.body || task.id}</span>
                  <span className={`status ${task.status || "unstarted"}`}>{task.status || "unstarted"}</span>
                </button>
              );
            })}
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
              <input value={title} onChange={(event) => updateTaskTitle(event.target.value)} disabled={!selectedTask} />
            </label>
            <label>
              Status
              <select value={status} onChange={(event) => updateTaskStatus(event.target.value)} disabled={!selectedTask}>
                {statusOptions.map((option) => (
                  <option key={option} value={option}>
                    {option}
                  </option>
                ))}
              </select>
            </label>
            <label className="wide">
              Body
              <textarea value={body} onChange={(event) => updateTaskBody(event.target.value)} disabled={!selectedTask} />
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
            <label className="wide">
              Request
              <textarea
                className="natural-request"
                value={newRequest}
                onChange={(event) => setNewRequest(event.target.value)}
                placeholder="Describe the task in your own words"
              />
            </label>
            <div className="actions wide">
              <button type="submit" disabled={saving || !newRequest.trim()}>
                Create
              </button>
            </div>
          </form>
        </section>

        <section className="orchestrator-dashboard" aria-label="Orchestrator console">
          <div className="section-heading">
            <h2>Orchestrator</h2>
            <div className="heading-status">
              <span>{selectedProjectSlug || "No project"}</span>
              <ConnectionBadge label="Orchestrator log" state={orchestratorConnection} />
            </div>
          </div>
          <div className="console-metadata">
            <span>Project: {selectedProjectSlug || "None"}</span>
            <span>Session: {tmuxSession}</span>
            <span>Window: {orchestratorWindow}</span>
          </div>
          <div className="log-panel">
            <div className="log-heading">
              <span>{orchestratorWindow}</span>
              <span>{selectedProject?.repo_path || "No repository"}</span>
            </div>
            <pre>{logDisplayText(
              orchestratorLog,
              orchestratorConnection,
              "No orchestrator output yet.",
              "Select a project to open the orchestrator log."
            )}</pre>
          </div>
          <form className="orchestrator-input-form" onSubmit={sendOrchestratorInput}>
            <label>
              Orchestrator input
              <textarea
                value={orchestratorInput}
                onChange={(event) => setOrchestratorInput(event.target.value)}
                disabled={!selectedProjectSlug || orchestratorInputSending}
              />
            </label>
            <div className="actions">
              <button
                type="submit"
                disabled={!selectedProjectSlug || orchestratorInputSending || !orchestratorInput.trim()}
              >
                Send Input
              </button>
            </div>
          </form>
        </section>

        <section className="worker-dashboard" aria-label="Worker dashboard">
          <div className="section-heading">
            <h2>Workers</h2>
            <div className="heading-status">
              <span>{workers.length} active</span>
              <ConnectionBadge label="Worker log" state={workerConnection} />
            </div>
          </div>
          <div className="worker-layout">
            <div className="worker-list">
              {workers.map((worker) => {
                const isSelected = worker.worker_id === selectedWorker?.worker_id;
                return (
                  <button
                    aria-current={isSelected ? "true" : undefined}
                    className={`worker-row ${isSelected ? "selected" : ""}`}
                    key={worker.worker_id}
                    type="button"
                    onClick={() => setSelectedWorkerID(worker.worker_id)}
                  >
                    <span className="task-title">{worker.worker_id}</span>
                    <span>{worker.harness || "worker"}</span>
                  </button>
                );
              })}
              {workers.length === 0 && <div className="empty">No active workers.</div>}
            </div>
            <div className="log-panel">
              <div className="log-heading">
                <span>{selectedWorker?.worker_id || "No worker selected"}</span>
                <span>{selectedWorkerTask?.branch || selectedWorker?.task_id || "No task"}</span>
              </div>
              <pre>{logDisplayText(
                workerLog,
                workerConnection,
                "No worker output yet.",
                "Select a worker to open its log stream."
              )}</pre>
            </div>
          </div>
          <form className="followup-form" onSubmit={sendWorkerFollowup}>
            <div className="console-metadata">
              <span>Project: {selectedProjectSlug || "None"}</span>
              <span>Session: {tmuxSession}</span>
              <span>Window: {selectedWorker?.worker_id || "None"}</span>
              <span>Task: {selectedWorker?.task_id || "None"}</span>
            </div>
            <label>
              Worker followup
              <textarea
                value={followupMessage}
                onChange={(event) => setFollowupMessage(event.target.value)}
                disabled={!selectedWorker || followupSending}
              />
            </label>
            <div className="actions">
              <button type="submit" disabled={!selectedWorker || followupSending || !followupMessage.trim()}>
                Send Followup
              </button>
            </div>
          </form>
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
            <label>
              Listen address
              <input
                value={configDraft.serverHost}
                onChange={(event) => updateConfigDraft({ serverHost: event.target.value })}
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
                tokenRef.current = tokenDraft;
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
    serverHost: "",
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

function emptyTaskEditorDirty(): TaskEditorDirty {
  return {
    title: false,
    body: false,
    status: false
  };
}

function idleConnection(detail: string): ConnectionState {
  return { phase: "idle", detail };
}

function openReconnectingWebSocket(options: ReconnectingWebSocketOptions) {
  let closed = false;
  let retryTimer: number | undefined;
  let stableTimer: number | undefined;
  let socket: WebSocket | undefined;
  let attempts = 0;

  const connect = () => {
    if (closed) {
      return;
    }
    let opened = false;
    options.setState({ phase: "connecting", detail: "Opening stream..." });
    socket = new WebSocket(webSocketURL(options.path, options.token));
    socket.addEventListener("open", () => {
      if (closed) {
        return;
      }
      opened = true;
      options.setState({ phase: "open", detail: "Connected." });
      stableTimer = window.setTimeout(() => {
        attempts = 0;
        stableTimer = undefined;
      }, stableOpenResetDelayMs);
    });
    socket.addEventListener("message", (event) => {
      if (!closed) {
        options.onMessage(event);
      }
    });
    socket.addEventListener("error", () => {
      if (!closed && opened) {
        options.onSocketError?.();
      }
    });
    socket.addEventListener("close", () => {
      if (closed) {
        return;
      }
      clearStableTimer();
      if (opened) {
        scheduleReconnect("Stream disconnected.");
        return;
      }
      void diagnoseWebSocketFailure(options.path, options.token).then((diagnosis) => {
        if (closed) {
          return;
        }
        if (!diagnosis.retryable) {
          options.setState({ phase: diagnosis.phase, detail: diagnosis.detail });
          return;
        }
        scheduleReconnect(diagnosis.detail, diagnosis.phase);
      });
    });
  };

  const scheduleReconnect = (detail: string, exhaustedPhase: FailureConnectionPhase = "failed") => {
    if (attempts >= reconnectDelaysMs.length) {
      options.setState({ phase: exhaustedPhase, detail: `${detail} Reconnect attempts exhausted.` });
      return;
    }
    const retryInMs = reconnectDelaysMs[attempts];
    attempts += 1;
    options.setState({
      phase: "retrying",
      detail,
      attempt: attempts,
      maxAttempts: reconnectDelaysMs.length,
      retryInMs
    });
    retryTimer = window.setTimeout(connect, retryInMs);
  };

  const clearStableTimer = () => {
    if (stableTimer !== undefined) {
      window.clearTimeout(stableTimer);
      stableTimer = undefined;
    }
  };

  connect();

  return () => {
    closed = true;
    if (retryTimer !== undefined) {
      window.clearTimeout(retryTimer);
    }
    clearStableTimer();
    socket?.close();
  };
}

async function diagnoseWebSocketFailure(path: string, token: string): Promise<WebSocketDiagnosis> {
  try {
    const response = await fetch(withTokenQuery(path, token), {
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(token ? { Authorization: `Bearer ${token}` } : {})
      }
    });
    const detail = await responseErrorDetail(response);
    if (response.status === 401) {
      return {
        phase: "unauthorized",
        detail: "Unauthorized. Apply a valid API token.",
        retryable: false
      };
    }
    if (response.status === 403) {
      return {
        phase: "forbidden",
        detail: detail || "Forbidden for this stream.",
        retryable: false
      };
    }
    if (response.status === 409) {
      return {
        phase: "blocked",
        detail: detail || "Another active log stream is already open.",
        retryable: true
      };
    }
    if (response.status === 404) {
      return {
        phase: "missing",
        detail: detail || "Stream not found.",
        retryable: false
      };
    }
    if (response.status >= 500) {
      return {
        phase: "failed",
        detail: detail || "Server failed before opening the stream.",
        retryable: true
      };
    }
    if (response.status === 400) {
      return {
        phase: "failed",
        detail: "WebSocket handshake did not complete.",
        retryable: false
      };
    }
    return {
      phase: "failed",
      detail: detail || "WebSocket handshake was rejected before opening.",
      retryable: false
    };
  } catch {
    return {
      phase: "failed",
      detail: "Server is unreachable.",
      retryable: true
    };
  }
}

async function responseErrorDetail(response: Response) {
  const payload = (await response
    .clone()
    .json()
    .catch(() => undefined)) as { error?: unknown } | undefined;
  if (payload && typeof payload.error === "string") {
    return payload.error;
  }
  const text = await response.text().catch(() => "");
  return text.trim() || response.statusText;
}

function appendLogEvent(event: MessageEvent, setLog: LogSetter) {
  try {
    const msg = JSON.parse(String(event.data)) as { type?: string; data?: string };
    if (msg.type === "line" && typeof msg.data === "string") {
      appendLogLine(setLog, msg.data);
    } else if (msg.type === "closed") {
      appendLogLine(setLog, "[stream closed]", false);
    } else if (msg.type === "error" && msg.data) {
      appendLogLine(setLog, `[error] ${msg.data}`, false);
    }
  } catch {
    appendLogLine(setLog, String(event.data));
  }
}

function appendLogLine(setLog: LogSetter, line: string, trim = true) {
  setLog((current) => [...(trim ? current.slice(-299) : current), line]);
}

function logDisplayText(lines: string[], state: ConnectionState, emptyText: string, idleText: string) {
  if (lines.length) {
    return lines.join("\n");
  }
  switch (state.phase) {
    case "idle":
      return state.detail || idleText;
    case "connecting":
      return "Connecting to stream...";
    case "open":
      return emptyText;
    case "retrying":
      return `${state.detail || "Stream disconnected."} Reconnecting in ${formatRetryDelay(state.retryInMs)} (${state.attempt}/${state.maxAttempts}).`;
    case "unauthorized":
    case "forbidden":
    case "blocked":
    case "missing":
    case "failed":
      return state.detail || connectionLabel(state);
  }
}

function ConnectionBadge({ label, state }: { label: string; state: ConnectionState }) {
  const text = connectionLabel(state);
  const detail = connectionDetail(state);
  return (
    <span
      aria-label={`${label} connection: ${detail}`}
      className={`connection-badge ${connectionTone(state.phase)}`}
      role="status"
      title={detail}
    >
      {text}
    </span>
  );
}

function connectionLabel(state: ConnectionState) {
  switch (state.phase) {
    case "idle":
      return "Idle";
    case "connecting":
      return "Connecting";
    case "open":
      return "Live";
    case "retrying":
      return `Reconnecting ${state.attempt}/${state.maxAttempts}`;
    case "unauthorized":
      return "Unauthorized";
    case "forbidden":
      return "Forbidden";
    case "blocked":
      return "Blocked";
    case "missing":
      return "Missing";
    case "failed":
      return "Disconnected";
  }
}

function connectionDetail(state: ConnectionState) {
  if (state.phase === "retrying") {
    return `${state.detail || "Stream disconnected."} Reconnecting in ${formatRetryDelay(state.retryInMs)} (${state.attempt}/${state.maxAttempts}).`;
  }
  return state.detail || connectionLabel(state);
}

function connectionTone(phase: ConnectionPhase) {
  switch (phase) {
    case "open":
      return "open";
    case "connecting":
    case "retrying":
      return "pending";
    case "unauthorized":
    case "forbidden":
    case "blocked":
    case "missing":
    case "failed":
      return "problem";
    case "idle":
      return "idle";
  }
}

function formatRetryDelay(delay?: number) {
  if (delay === undefined) {
    return "soon";
  }
  if (delay < 1000) {
    return `${delay}ms`;
  }
  return `${delay / 1000}s`;
}

function configToDraft(config: ConfigResponse): ConfigDraft {
  return {
    projectSlug: config.project.slug,
    repoPath: config.project.repo_path,
    worktreeBase: config.project.worktree_base,
    serverHost: config.server.host || "127.0.0.1",
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
      host: draft.serverHost,
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

function normalizedProjectSelection(projects: ProjectInfo[], currentSlug: string, preferredSlug = "") {
  if (preferredSlug && projects.some((project) => project.slug === preferredSlug)) {
    return preferredSlug;
  }
  if (projects.some((project) => project.slug === currentSlug)) {
    return currentSlug;
  }
  return projects[0]?.slug || "";
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

function ledgerWSPath(projectSlug: string) {
  return projectSlug ? `/ws/projects/${encodeURIComponent(projectSlug)}/ledger` : "/ws/ledger";
}

function orchestratorLogPath(projectSlug: string) {
  return projectSlug ? `/ws/projects/${encodeURIComponent(projectSlug)}/orchestrator` : "/ws/orchestrator";
}

function orchestratorInputPath(projectSlug: string) {
  return `/api/projects/${encodeURIComponent(projectSlug)}/orchestrator/input`;
}

function workerFollowupPath(projectSlug: string, workerID: string) {
  return projectSlug
    ? `/api/projects/${encodeURIComponent(projectSlug)}/workers/${encodeURIComponent(workerID)}/followup`
    : `/api/workers/${encodeURIComponent(workerID)}/followup`;
}

function webSocketURL(path: string, token: string) {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}${withTokenQuery(path, token)}`;
}

function withTokenQuery(path: string, token: string) {
  if (!token) {
    return path;
  }
  const separator = path.includes("?") ? "&" : "?";
  return `${path}${separator}token=${encodeURIComponent(token)}`;
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
