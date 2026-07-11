import {
  ApiError,
  type FetchLike,
  probeWebSocketEndpoint,
  terminalWebSocketUrl,
  projectTerminalWebSocketPath
} from "./api";

export type TerminalPhase = "idle" | "connecting" | "open" | "retrying" | "missing" | "unauthorized" | "failed";

export type TerminalState =
  | { phase: "idle"; detail?: string }
  | { phase: "connecting"; detail?: string }
  | { phase: "open"; detail?: string }
  | { phase: "missing" | "unauthorized" | "failed"; detail: string; status?: number; error?: ApiError }
  | { phase: "retrying"; detail: string; attempt: number; retryInMs: number; status?: number; error?: ApiError };

export type TerminalSize = { cols: number; rows: number };

export type RetryPolicy = {
  baseMs: number;
  maxMs: number;
  jitterRatio: number;
};

export const defaultRetryPolicy: RetryPolicy = {
  baseMs: 250,
  maxMs: 10_000,
  jitterRatio: 0.25
};

export type TimerApi = {
  setTimeout(callback: () => void, delayMs: number): ReturnType<typeof setTimeout>;
  clearTimeout(handle: ReturnType<typeof setTimeout>): void;
};

export type WakeTarget = {
  addEventListener(type: string, listener: () => void): void;
  removeEventListener(type: string, listener: () => void): void;
};

export type TerminalSocket = {
  readonly readyState: number;
  addEventListener(type: "open" | "message" | "error" | "close", listener: (event: Event) => void): void;
  removeEventListener(type: "open" | "message" | "error" | "close", listener: (event: Event) => void): void;
  send(data: string): void;
  close(): void;
};

export type TerminalSocketFactory = (url: string) => TerminalSocket;

export type TerminalControllerCallbacks = {
  onStateChange?: (state: TerminalState) => void;
  onData?: (data: string) => void;
  onError?: (error: Error) => void;
};

export type TerminalControllerOptions = {
  projectSlug: string;
  windowName: string;
  token?: string;
  baseUrl?: string;
  fetch?: FetchLike;
  websocketFactory?: TerminalSocketFactory;
  location?: Pick<Location, "protocol" | "host">;
  timers?: TimerApi;
  random?: () => number;
  retry?: Partial<RetryPolicy>;
  stableOpenMs?: number;
  probeTimeoutMs?: number;
  wakeTarget?: WakeTarget | null;
  visibilityTarget?: WakeTarget | null;
  isVisible?: () => boolean;
  callbacks?: TerminalControllerCallbacks;
};

export function calculateRetryDelay(attempt: number, policy: RetryPolicy = defaultRetryPolicy, random = Math.random): number {
  const safeAttempt = Math.max(0, Math.floor(attempt));
  const base = Math.max(0, policy.baseMs);
  const maximum = Math.max(base, policy.maxMs);
  const exponential = Math.min(maximum, base * 2 ** Math.min(safeAttempt, 30));
  const ratio = Math.min(1, Math.max(0, policy.jitterRatio));
  const randomValue = Math.min(1, Math.max(0, random()));
  return Math.round(exponential * (1 - ratio + randomValue * ratio));
}

const CONNECTING = 0;
const OPEN = 1;

const browserTimers: TimerApi = {
  setTimeout: (callback, delayMs) => globalThis.setTimeout(callback, delayMs),
  clearTimeout: (handle) => globalThis.clearTimeout(handle)
};

function browserWakeTarget(): WakeTarget | null {
  return typeof window === "undefined" ? null : window;
}

function browserVisibilityTarget(): WakeTarget | null {
  return typeof document === "undefined" ? null : document;
}

function browserVisibility(): boolean {
  return typeof document === "undefined" || document.visibilityState === "visible";
}

function browserWebSocketFactory(url: string): TerminalSocket {
  return new WebSocket(url);
}

type ResolvedRuntimeOptions = {
  baseUrl: string;
  fetcher?: FetchLike;
  websocketFactory: TerminalSocketFactory;
  location?: Pick<Location, "protocol" | "host">;
  timers: TimerApi;
  random: () => number;
  retryPolicy: RetryPolicy;
  stableOpenMs: number;
  probeTimeoutMs: number;
  wakeTarget: WakeTarget | null;
  visibilityTarget: WakeTarget | null;
  isVisible: () => boolean;
};

function resolveRuntimeOptions(options: TerminalControllerOptions): ResolvedRuntimeOptions {
  return {
    baseUrl: options.baseUrl ?? "",
    fetcher: options.fetch,
    websocketFactory: options.websocketFactory ?? browserWebSocketFactory,
    location: options.location,
    timers: options.timers ?? browserTimers,
    random: options.random ?? Math.random,
    retryPolicy: { ...defaultRetryPolicy, ...options.retry },
    stableOpenMs: Math.max(0, options.stableOpenMs ?? 10_000),
    probeTimeoutMs: Math.max(1, options.probeTimeoutMs ?? 5_000),
    wakeTarget: options.wakeTarget === undefined ? browserWakeTarget() : options.wakeTarget,
    visibilityTarget: options.visibilityTarget === undefined ? browserVisibilityTarget() : options.visibilityTarget,
    isVisible: options.isVisible ?? browserVisibility
  };
}

function sameLocation(
  left?: Pick<Location, "protocol" | "host">,
  right?: Pick<Location, "protocol" | "host">
): boolean {
  return left?.protocol === right?.protocol && left?.host === right?.host;
}

export class TerminalController {
  private projectSlug: string;
  private windowName: string;
  private token: string;
  private baseUrl: string;
  private fetcher?: FetchLike;
  private websocketFactory: TerminalSocketFactory;
  private location?: Pick<Location, "protocol" | "host">;
  private timers: TimerApi;
  private random: () => number;
  private retryPolicy: RetryPolicy;
  private stableOpenMs: number;
  private probeTimeoutMs: number;
  private wakeTarget: WakeTarget | null;
  private visibilityTarget: WakeTarget | null;
  private isVisible: () => boolean;
  private callbacks: TerminalControllerCallbacks;
  private state: TerminalState = { phase: "idle", detail: "Terminal is idle." };
  private readonly listeners = new Set<() => void>();
  private started = false;
  private generation = 0;
  private socketAttempt = 0;
  private retryAttempt = 0;
  private socket: TerminalSocket | undefined;
  private socketIdentity: { generation: number; attempt: number } | undefined;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private stableTimer: ReturnType<typeof setTimeout> | undefined;
  private probeAbort: AbortController | undefined;
  private probeTimer: ReturnType<typeof setTimeout> | undefined;
  private lastSize: TerminalSize | undefined;

  constructor(options: TerminalControllerOptions) {
    this.projectSlug = options.projectSlug;
    this.windowName = options.windowName;
    this.token = options.token ?? "";
    const runtime = resolveRuntimeOptions(options);
    this.baseUrl = runtime.baseUrl;
    this.fetcher = runtime.fetcher;
    this.websocketFactory = runtime.websocketFactory;
    this.location = runtime.location;
    this.timers = runtime.timers;
    this.random = runtime.random;
    this.retryPolicy = runtime.retryPolicy;
    this.stableOpenMs = runtime.stableOpenMs;
    this.probeTimeoutMs = runtime.probeTimeoutMs;
    this.wakeTarget = runtime.wakeTarget;
    this.visibilityTarget = runtime.visibilityTarget;
    this.isVisible = runtime.isVisible;
    this.callbacks = options.callbacks ?? {};
  }

  getSnapshot = (): TerminalState => this.state;

  getGeneration(): number {
    return this.generation;
  }

  getTarget(): { projectSlug: string; windowName: string } {
    return { projectSlug: this.projectSlug, windowName: this.windowName };
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  setCallbacks(callbacks: TerminalControllerCallbacks): void {
    this.callbacks = callbacks;
  }

  updateOptions(options: TerminalControllerOptions): void {
    const nextRuntime = resolveRuntimeOptions(options);
    const targetChanged = this.projectSlug !== options.projectSlug ||
      this.windowName !== options.windowName || this.token !== (options.token ?? "");
    const runtimeChanged = !this.runtimeMatches(nextRuntime);
    if (!targetChanged && !runtimeChanged) {
      return;
    }
    if (this.started) {
      this.removeWakeListeners();
    }
    this.invalidateGeneration();
    this.projectSlug = options.projectSlug;
    this.windowName = options.windowName;
    this.token = options.token ?? "";
    this.baseUrl = nextRuntime.baseUrl;
    this.fetcher = nextRuntime.fetcher;
    this.websocketFactory = nextRuntime.websocketFactory;
    this.location = nextRuntime.location;
    this.timers = nextRuntime.timers;
    this.random = nextRuntime.random;
    this.retryPolicy = nextRuntime.retryPolicy;
    this.stableOpenMs = nextRuntime.stableOpenMs;
    this.probeTimeoutMs = nextRuntime.probeTimeoutMs;
    this.wakeTarget = nextRuntime.wakeTarget;
    this.visibilityTarget = nextRuntime.visibilityTarget;
    this.isVisible = nextRuntime.isVisible;
    if (!this.started || !this.hasTarget()) {
      this.setState({ phase: "idle", detail: this.hasTarget() ? "Terminal is idle." : "Select a terminal." });
      return;
    }
    this.installWakeListeners();
    this.connect(this.generation);
  }

  configure(projectSlug: string, windowName: string, token = ""): void {
    if (this.projectSlug === projectSlug && this.windowName === windowName && this.token === token) {
      return;
    }
    this.projectSlug = projectSlug;
    this.windowName = windowName;
    this.token = token;
    this.invalidateGeneration();
    if (!this.started || !this.hasTarget()) {
      this.setState({ phase: "idle", detail: this.hasTarget() ? "Terminal is idle." : "Select a terminal." });
      return;
    }
    this.connect(this.generation);
  }

  start(): void {
    if (this.started) {
      return;
    }
    this.started = true;
    this.installWakeListeners();
    if (this.hasTarget()) {
      this.connect(this.generation);
    } else {
      this.setState({ phase: "idle", detail: "Select a terminal." });
    }
  }

  stop(): void {
    if (!this.started && !this.socket && this.retryTimer === undefined) {
      return;
    }
    this.started = false;
    this.generation += 1;
    this.clearTimers();
    this.removeWakeListeners();
    this.closeSocket();
    this.setStateSilently({ phase: "idle", detail: "Terminal is idle." });
  }

  retry(): void {
    if (!this.hasTarget()) {
      this.setState({ phase: "idle", detail: "Select a terminal." });
      return;
    }
    if (!this.started) {
      this.start();
      return;
    }
    this.invalidateGeneration();
    this.retryAttempt = 0;
    this.connect(this.generation);
  }

  wake(): void {
    if (this.started && this.state.phase !== "open" && this.state.phase !== "connecting") {
      this.retry();
    }
  }

  sendInput(data: string, expectedGeneration = this.generation): boolean {
    return this.sendFrame({ type: "input", data }, expectedGeneration);
  }

  resize(cols: number, rows: number, expectedGeneration = this.generation): boolean {
    if (!Number.isInteger(cols) || !Number.isInteger(rows) || cols <= 0 || rows <= 0 || expectedGeneration !== this.generation) {
      return false;
    }
    this.lastSize = { cols, rows };
    return this.sendFrame({ type: "resize", cols, rows }, expectedGeneration);
  }

  private hasTarget(): boolean {
    return this.projectSlug.trim() !== "" && this.windowName.trim() !== "";
  }

  private runtimeMatches(next: ResolvedRuntimeOptions): boolean {
    return this.baseUrl === next.baseUrl && this.fetcher === next.fetcher &&
      this.websocketFactory === next.websocketFactory && sameLocation(this.location, next.location) &&
      this.timers === next.timers && this.random === next.random &&
      this.retryPolicy.baseMs === next.retryPolicy.baseMs && this.retryPolicy.maxMs === next.retryPolicy.maxMs &&
      this.retryPolicy.jitterRatio === next.retryPolicy.jitterRatio && this.stableOpenMs === next.stableOpenMs &&
      this.probeTimeoutMs === next.probeTimeoutMs && this.wakeTarget === next.wakeTarget &&
      this.visibilityTarget === next.visibilityTarget && this.isVisible === next.isVisible;
  }

  private invalidateGeneration(): void {
    this.generation += 1;
    this.clearTimers();
    this.closeSocket();
  }

  private connect(generation: number): void {
    if (!this.started || !this.hasTarget() || generation !== this.generation) {
      return;
    }
    if (this.socket && (this.socket.readyState === CONNECTING || this.socket.readyState === OPEN)) {
      return;
    }
    this.clearRetryTimer();
    this.setStateForGeneration({ phase: "connecting", detail: "Opening terminal stream..." }, generation);
    const attempt = ++this.socketAttempt;
    let socket: TerminalSocket;
    try {
      socket = this.websocketFactory(
        terminalWebSocketUrl(this.projectSlug, this.windowName, this.token, this.location, this.baseUrl)
      );
    } catch (error) {
      this.scheduleRetry(generation, "Unable to open terminal stream.", errorToApiError(error));
      return;
    }
    this.socket = socket;
    this.socketIdentity = { generation, attempt };
    const guard = () => this.isCurrentSocket(socket, generation, attempt);
    const onOpen = (_event: Event) => {
      if (!guard()) return;
      this.clearStableTimer();
      this.setStateForGeneration({ phase: "open", detail: "Terminal stream connected." }, generation);
      this.replayResize(generation, socket);
      this.stableTimer = this.timers.setTimeout(() => {
        if (guard()) {
          this.retryAttempt = 0;
          this.stableTimer = undefined;
        }
      }, this.stableOpenMs);
    };
    const onMessage = (event: Event) => {
      if (!guard()) return;
      this.handleMessage(event, guard);
    };
    const onError = (_event: Event) => {
      if (!guard()) return;
      this.callbacks.onError?.(new Error("Terminal websocket error."));
      try {
        socket.close();
      } catch {
        // The close event remains the normal retry trigger; a failed close is harmless here.
      }
    };
    const onClose = () => {
      if (!guard()) return;
      const opened = socket.readyState === OPEN || this.state.phase === "open";
      this.detachSocket(socket, onOpen, onMessage, onError, onClose);
      this.socketCleanup = undefined;
      this.socket = undefined;
      this.socketIdentity = undefined;
      this.clearStableTimer();
      if (opened) {
        this.scheduleRetry(generation, "Terminal stream disconnected.");
      } else {
        void this.diagnoseBeforeRetry(generation);
      }
    };
    socket.addEventListener("open", onOpen);
    socket.addEventListener("message", onMessage);
    socket.addEventListener("error", onError);
    socket.addEventListener("close", onClose);
    this.socketCleanup = () => this.detachSocket(socket, onOpen, onMessage, onError, onClose);
  }

  private handleMessage(event: Event, guard: () => boolean): void {
    const data = "data" in event ? (event.data as unknown) : undefined;
    if (typeof data !== "string") {
      this.callbacks.onError?.(new Error("Terminal websocket message was not text."));
      return;
    }
    let message: unknown;
    try {
      message = JSON.parse(data) as unknown;
    } catch {
      this.callbacks.onError?.(new Error("Terminal websocket message was not valid JSON."));
      return;
    }
    if (!isRecord(message) || typeof message.type !== "string" || !guard()) {
      return;
    }
    if ((message.type === "chunk" || message.type === "line") && typeof message.data === "string") {
      this.callbacks.onData?.(message.data);
    } else if (message.type === "error" && typeof message.data === "string") {
      this.callbacks.onError?.(new Error(message.data));
    }
  }

  private replayResize(generation: number, socket: TerminalSocket): void {
    if (this.lastSize && generation === this.generation && this.socket === socket && socket.readyState === OPEN) {
      try {
        socket.send(JSON.stringify({ type: "resize", ...this.lastSize }));
      } catch (error) {
        this.callbacks.onError?.(errorToApiError(error));
        safeClose(socket);
      }
    }
  }

  private sendFrame(frame: Record<string, string | number>, expectedGeneration: number): boolean {
    const socket = this.socket;
    if (expectedGeneration !== this.generation || !socket || socket !== this.socket || socket.readyState !== OPEN) {
      return false;
    }
    try {
      socket.send(JSON.stringify(frame));
      return true;
    } catch (error) {
      this.callbacks.onError?.(errorToApiError(error));
      safeClose(socket);
      return false;
    }
  }

  private scheduleRetry(generation: number, detail: string, error?: ApiError): void {
    if (!this.started || generation !== this.generation || !this.hasTarget()) return;
    this.clearRetryTimer();
    const attempt = ++this.retryAttempt;
    const retryInMs = calculateRetryDelay(attempt - 1, this.retryPolicy, this.random);
    this.setStateForGeneration({ phase: "retrying", detail, attempt, retryInMs, status: error?.status, error }, generation);
    this.retryTimer = this.timers.setTimeout(() => {
      if (generation === this.generation) {
        this.retryTimer = undefined;
        this.connect(generation);
      }
    }, retryInMs);
  }

  private async diagnoseBeforeRetry(generation: number): Promise<void> {
    this.probeAbort?.abort();
    const abort = new AbortController();
    this.probeAbort = abort;
    let timedOut = false;
    const probeTimer = this.timers.setTimeout(() => {
      timedOut = true;
      abort.abort();
    }, this.probeTimeoutMs);
    this.probeTimer = probeTimer;
    try {
      await probeWebSocketEndpoint(projectTerminalWebSocketPath(this.projectSlug, this.windowName), {
        baseUrl: this.baseUrl,
        token: this.token,
        fetch: this.fetcher,
        signal: abort.signal
      });
      if (this.isCurrentGeneration(generation)) {
        this.scheduleRetry(generation, "Terminal handshake did not complete.");
      }
    } catch (error) {
      if (!this.isCurrentGeneration(generation) || (abort.signal.aborted && !timedOut)) return;
      const apiError = error instanceof ApiError ? error : undefined;
      if (apiError?.status === 401) {
        this.setStateForGeneration({ phase: "unauthorized", detail: apiError.message, status: apiError.status, error: apiError }, generation);
        return;
      }
      if (apiError?.status === 404) {
        this.setStateForGeneration({ phase: "missing", detail: apiError.message, status: apiError.status, error: apiError }, generation);
        return;
      }
      if (apiError && !isRetryableStatus(apiError.status)) {
        this.setStateForGeneration({ phase: "failed", detail: apiError.message, status: apiError.status, error: apiError }, generation);
        return;
      }
      this.scheduleRetry(generation, apiError?.message ?? "Terminal server is unreachable.", apiError);
    } finally {
      if (this.probeTimer === probeTimer) {
        this.timers.clearTimeout(probeTimer);
        this.probeTimer = undefined;
      }
      if (this.probeAbort === abort) this.probeAbort = undefined;
    }
  }

  private isCurrentGeneration(generation: number): boolean {
    return this.started && generation === this.generation;
  }

  private isCurrentSocket(socket: TerminalSocket, generation: number, attempt: number): boolean {
    return this.isCurrentGeneration(generation) && this.socket === socket &&
      this.socketIdentity?.generation === generation && this.socketIdentity.attempt === attempt;
  }

  private setState(state: TerminalState): void {
    this.state = state;
    this.callbacks.onStateChange?.(state);
    for (const listener of this.listeners) listener();
  }

  private setStateSilently(state: TerminalState): void {
    this.state = state;
    for (const listener of this.listeners) listener();
  }

  private setStateForGeneration(state: TerminalState, generation: number): void {
    if (this.isCurrentGeneration(generation)) this.setState(state);
  }

  private clearRetryTimer(): void {
    if (this.retryTimer !== undefined) {
      this.timers.clearTimeout(this.retryTimer);
      this.retryTimer = undefined;
    }
  }

  private clearStableTimer(): void {
    if (this.stableTimer !== undefined) {
      this.timers.clearTimeout(this.stableTimer);
      this.stableTimer = undefined;
    }
  }

  private clearTimers(): void {
    this.clearRetryTimer();
    this.clearStableTimer();
    this.probeAbort?.abort();
    this.probeAbort = undefined;
    if (this.probeTimer !== undefined) {
      this.timers.clearTimeout(this.probeTimer);
      this.probeTimer = undefined;
    }
  }

  private socketCleanup: (() => void) | undefined;

  private closeSocket(): void {
    const socket = this.socket;
    this.socketCleanup?.();
    this.socketCleanup = undefined;
    this.socket = undefined;
    this.socketIdentity = undefined;
    if (socket) safeClose(socket);
  }

  private detachSocket(
    socket: TerminalSocket,
    onOpen: (event: Event) => void,
    onMessage: (event: Event) => void,
    onError: (event: Event) => void,
    onClose: (event: Event) => void
  ): void {
    socket.removeEventListener("open", onOpen);
    socket.removeEventListener("message", onMessage);
    socket.removeEventListener("error", onError);
    socket.removeEventListener("close", onClose);
  }

  private installWakeListeners(): void {
    this.wakeTarget?.addEventListener("online", this.handleOnline);
    this.visibilityTarget?.addEventListener("visibilitychange", this.handleVisibility);
  }

  private removeWakeListeners(): void {
    this.wakeTarget?.removeEventListener("online", this.handleOnline);
    this.visibilityTarget?.removeEventListener("visibilitychange", this.handleVisibility);
  }

  private readonly handleOnline = (): void => this.wake();
  private readonly handleVisibility = (): void => {
    if (this.isVisible()) this.wake();
  };
}

function isRetryableStatus(status: number): boolean {
  // A plain HTTP probe cannot complete the websocket Upgrade handshake, so
  // the terminal route's expected response is 400. Treat it as transient;
  // authentication and missing-window statuses remain terminal states.
  return status === 400 || status === 408 || status === 409 || status === 425 || status === 429 || status >= 500;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function errorToApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error;
  return new ApiError(error instanceof Error ? error.message : "Terminal websocket failed.", 0, "", error);
}

function safeClose(socket: TerminalSocket): void {
  try {
    socket.close();
  } catch {
    // Cleanup must remain idempotent even when a websocket implementation rejects close().
  }
}
