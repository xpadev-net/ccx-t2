import { FormEvent, StrictMode, useEffect, useMemo, useState } from "react";
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
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

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

  useEffect(() => {
    void refreshTasks();
    void refreshWorkers();
  }, []);

  useEffect(() => {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const tokenQuery = token ? `?token=${encodeURIComponent(token)}` : "";
    const socket = new WebSocket(`${protocol}//${window.location.host}/ws/ledger${tokenQuery}`);
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
  }, [token]);

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
        `${protocol}//${window.location.host}/ws/worker/${encodeURIComponent(selectedWorker.worker_id)}${tokenQuery}`
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
  }, [selectedWorker?.worker_id, token]);

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
    if (showLoading) {
      setLoading(true);
    }
    setError("");
    try {
      const data = await api<Task[]>("/api/tasks", {}, authToken);
      setTasks(data);
      setSelectedID((current) => current || data[0]?.id || "");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function refreshWorkers(authToken = token) {
    try {
      const data = await api<WorkerInfo[]>("/api/workers", {}, authToken);
      setWorkers(data);
      setSelectedWorkerID((current) => current || data[0]?.worker_id || "");
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  async function refreshAll(showLoading = true, authToken = token) {
    await Promise.all([refreshTasks(showLoading, authToken), refreshWorkers(authToken)]);
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const response = await api<TaskCreateResponse>(
        "/api/tasks",
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
        `/api/tasks/${encodeURIComponent(selectedTask.id)}`,
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
        `/api/tasks/${encodeURIComponent(selectedTask.id)}`,
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

  return (
    <main className="app-shell">
      <header className="topbar">
        <div>
          <h1>ccx-t2</h1>
          <p>Task ledger and worker orchestration</p>
        </div>
        <button className="secondary" type="button" onClick={() => void refreshAll()} disabled={loading}>
          Refresh
        </button>
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
                void refreshAll(true, tokenDraft);
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
