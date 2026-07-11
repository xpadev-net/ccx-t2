export type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type ProjectInfo = {
  slug: string;
  repo_path: string;
  session?: string;
};

export type TerminalKind = "shell" | "orchestrator" | "worker";

export type TerminalInfo = {
  id: string;
  window: string;
  title: string;
  kind: TerminalKind;
  active: boolean;
  available: boolean;
  closable: boolean;
};

export type DeleteTerminalResponse = {
  deleted: boolean;
  window: string;
};

export type ApiClientOptions = {
  baseUrl?: string;
  token?: string;
  fetch?: FetchLike;
};

export type ApiRequestOptions = ApiClientOptions & {
  signal?: AbortSignal;
};

export type ApiOptions = string | ApiRequestOptions;

type JsonObject = Record<string, unknown>;

/** An HTTP failure that keeps both the status and server-provided details. */
export class ApiError extends Error {
  readonly status: number;
  readonly statusText: string;
  readonly details: unknown;
  readonly payload: unknown;

  constructor(message: string, status: number, statusText: string, details: unknown = undefined, payload: unknown = undefined) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.statusText = statusText;
    this.details = details;
    this.payload = payload;
  }
}

export class TerminalApiClient {
  private readonly options: ApiRequestOptions;

  constructor(options: ApiRequestOptions = {}) {
    this.options = { ...options };
  }

  async listProjects(signal?: AbortSignal): Promise<ProjectInfo[]> {
    const payload = await requestJson<unknown>("/api/projects", { ...this.options, signal });
    return parseArray(payload, parseProjectInfo, "projects");
  }

  async listProjectTerminals(projectSlug: string, signal?: AbortSignal): Promise<TerminalInfo[]> {
    const payload = await requestJson<unknown>(projectTerminalsPath(projectSlug), { ...this.options, signal });
    return parseArray(payload, parseTerminalInfo, "terminals");
  }

  async createProjectTerminal(projectSlug: string, signal?: AbortSignal): Promise<TerminalInfo> {
    const payload = await requestJson<unknown>(projectTerminalsPath(projectSlug), {
      ...this.options,
      signal,
      init: { method: "POST" }
    });
    return parseTerminalInfo(payload, "terminal");
  }

  async deleteProjectTerminal(projectSlug: string, windowName: string, signal?: AbortSignal): Promise<DeleteTerminalResponse> {
    const payload = await requestJson<unknown>(projectTerminalPath(projectSlug, windowName), {
      ...this.options,
      signal,
      init: { method: "DELETE" }
    });
    if (!isRecord(payload) || typeof payload.deleted !== "boolean" || typeof payload.window !== "string") {
      throw invalidPayload("delete terminal", payload);
    }
    return { deleted: payload.deleted, window: payload.window };
  }
}

export function createTerminalApiClient(options: ApiRequestOptions = {}): TerminalApiClient {
  return new TerminalApiClient(options);
}

export function listProjects(options: ApiOptions = {}): Promise<ProjectInfo[]> {
  const normalized = normalizeOptions(options);
  return new TerminalApiClient(normalized).listProjects(normalized.signal);
}

export function listProjectTerminals(projectSlug: string, options: ApiOptions = {}): Promise<TerminalInfo[]> {
  const normalized = normalizeOptions(options);
  return new TerminalApiClient(normalized).listProjectTerminals(projectSlug, normalized.signal);
}

export function createProjectTerminal(projectSlug: string, options: ApiOptions = {}): Promise<TerminalInfo> {
  const normalized = normalizeOptions(options);
  return new TerminalApiClient(normalized).createProjectTerminal(projectSlug, normalized.signal);
}

export function deleteProjectTerminal(
  projectSlug: string,
  windowName: string,
  options: ApiOptions = {}
): Promise<DeleteTerminalResponse> {
  const normalized = normalizeOptions(options);
  return new TerminalApiClient(normalized).deleteProjectTerminal(projectSlug, windowName, normalized.signal);
}

/** Fetches a websocket route over HTTP so handshake failures retain status/details. */
export async function probeWebSocketEndpoint(path: string, options: ApiRequestOptions = {}): Promise<void> {
  await requestJson<unknown>(path, {
    ...options,
    init: { method: "GET", cache: "no-store", headers: { Accept: "application/json" } }
  });
}

export function projectTerminalsPath(projectSlug: string): string {
  return `/api/projects/${encodeURIComponent(projectSlug)}/terminals`;
}

export function projectTerminalPath(projectSlug: string, windowName: string): string {
  return `${projectTerminalsPath(projectSlug)}/${encodeURIComponent(windowName)}`;
}

export function projectTerminalWebSocketPath(projectSlug: string, windowName: string): string {
  return `/ws/projects/${encodeURIComponent(projectSlug)}/terminal/${encodeURIComponent(windowName)}`;
}

export function websocketUrl(path: string, token = "", locationLike?: Pick<Location, "protocol" | "host">, baseUrl = ""): string {
  const withToken = addTokenQuery(path, token);
  const location = locationLike ?? currentLocation();
  if (baseUrl) {
    const parsed = new URL(baseUrl, location ? `${location.protocol}//${location.host}` : undefined);
    const protocol = parsed.protocol === "https:" ? "wss:" : "ws:";
    const basePath = parsed.pathname.replace(/\/$/, "");
    return `${protocol}//${parsed.host}${basePath}${withToken}`;
  }
  if (!location) {
    return withToken;
  }
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${location.host}${withToken}`;
}

export function terminalWebSocketUrl(
  projectSlug: string,
  windowName: string,
  token = "",
  locationLike?: Pick<Location, "protocol" | "host">,
  baseUrl = ""
): string {
  return websocketUrl(projectTerminalWebSocketPath(projectSlug, windowName), token, locationLike, baseUrl);
}

export function addTokenQuery(path: string, token: string): string {
  if (!token) {
    return path;
  }
  const separator = path.includes("?") ? "&" : "?";
  return `${path}${separator}token=${encodeURIComponent(token)}`;
}

export async function requestJson<T>(path: string, options: ApiRequestOptions & { init?: RequestInit } = {}): Promise<T> {
  const fetcher = options.fetch ?? globalThis.fetch;
  if (!fetcher) {
    throw new Error("fetch is not available");
  }
  const { init, ...requestOptions } = options;
  const response = await fetcher(resolveUrl(path, requestOptions.baseUrl), {
    ...init,
    signal: requestOptions.signal,
    headers: {
      "Content-Type": "application/json",
      ...(requestOptions.token ? { Authorization: `Bearer ${requestOptions.token}` } : {}),
      ...(init?.headers ?? {})
    }
  });
  const payload = await readJson(response);
  if (!response.ok) {
    throw apiErrorFromResponse(response, payload);
  }
  return payload as T;
}

function normalizeOptions(options: ApiOptions): ApiRequestOptions {
  return typeof options === "string" ? { token: options } : options;
}

function resolveUrl(path: string, baseUrl = ""): string {
  if (!baseUrl) {
    return path;
  }
  const parsed = new URL(baseUrl);
  const basePath = parsed.pathname.replace(/\/$/, "");
  const requestPath = path.startsWith("/") ? path : `/${path}`;
  return new URL(`${basePath}${requestPath}`, parsed.origin).toString();
}

function currentLocation(): Pick<Location, "protocol" | "host"> | undefined {
  return typeof window === "undefined" ? undefined : window.location;
}

async function readJson(response: Response): Promise<unknown> {
  if (response.status === 204) {
    return undefined;
  }
  const text = await response.text();
  if (!text.trim()) {
    return undefined;
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

function apiErrorFromResponse(response: Response, payload: unknown): ApiError {
  const record = isRecord(payload) ? payload : undefined;
  const message = stringValue(record?.error) ?? stringValue(record?.message) ??
    (typeof payload === "string" ? payload : response.statusText || `HTTP ${response.status}`);
  const details = record?.details ?? record?.detail ?? payload;
  return new ApiError(message, response.status, response.statusText, details, payload);
}

function parseArray<T>(payload: unknown, parse: (value: unknown, label: string) => T, label: string): T[] {
  if (!Array.isArray(payload)) {
    throw invalidPayload(label, payload);
  }
  return payload.map((value, index) => parse(value, `${label}[${index}]`));
}

function parseProjectInfo(value: unknown, label: string): ProjectInfo {
  if (!isRecord(value) || typeof value.slug !== "string" || typeof value.repo_path !== "string") {
    throw invalidPayload(label, value);
  }
  const session = value.session;
  if (session !== undefined && typeof session !== "string") {
    throw invalidPayload(label, value);
  }
  return { slug: value.slug, repo_path: value.repo_path, ...(typeof session === "string" ? { session } : {}) };
}

function parseTerminalInfo(value: unknown, label: string): TerminalInfo {
  if (!isRecord(value) || typeof value.id !== "string" || typeof value.window !== "string" ||
      typeof value.title !== "string" || !isTerminalKind(value.kind) || typeof value.active !== "boolean" ||
      typeof value.available !== "boolean" || typeof value.closable !== "boolean") {
    throw invalidPayload(label, value);
  }
  return {
    id: value.id,
    window: value.window,
    title: value.title,
    kind: value.kind,
    active: value.active,
    available: value.available,
    closable: value.closable
  };
}

function isTerminalKind(value: unknown): value is TerminalKind {
  return value === "shell" || value === "orchestrator" || value === "worker";
}

function isRecord(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function invalidPayload(label: string, payload: unknown): ApiError {
  return new ApiError(`Invalid ${label} response`, 200, "OK", payload, payload);
}
