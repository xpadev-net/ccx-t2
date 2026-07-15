import { TerminalController, type TerminalSocket } from "./controller";

class ProbeSocket implements TerminalSocket {
  readyState = 0;
  private readonly listeners = new Map<string, Set<(event: Event) => void>>();

  addEventListener(type: "open" | "message" | "error" | "close", listener: (event: Event) => void): void {
    const listeners = this.listeners.get(type) ?? new Set<(event: Event) => void>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: "open" | "message" | "error" | "close", listener: (event: Event) => void): void {
    this.listeners.get(type)?.delete(listener);
  }

  send(): void {}

  close(): void {
    this.readyState = 3;
    this.emit("close", new Event("close"));
  }

  open(): void {
    this.readyState = 1;
    this.emit("open", new Event("open"));
  }

  message(value: unknown): void {
    const event = new Event("message") as Event & { data: string };
    Object.defineProperty(event, "data", { value: JSON.stringify(value) });
    this.emit("message", event);
  }

  private emit(type: string, event: Event): void {
    for (const listener of [...(this.listeners.get(type) ?? [])]) {
      listener(event);
    }
  }
}

// Bundled and executed by the focused lifecycle validation command. Keeping the
// probe beside the controller lets TypeScript compile the regression without
// adding a test framework or changing the package manifest.
export function probeSnapshotBoundaryAndStaleIdentity(): void {
  const sockets: ProbeSocket[] = [];
  const received: string[] = [];
  const controller = new TerminalController({
    projectSlug: "alpha",
    windowName: "alpha-shell-1",
    token: "token-a",
    websocketFactory: () => {
      const socket = new ProbeSocket();
      sockets.push(socket);
      return socket;
    },
    wakeTarget: null,
    visibilityTarget: null,
    callbacks: {
      onSnapshot: (data, identity) => received.push(`snapshot:${identity.projectSlug}:${identity.token}:${data}`),
      onData: (data, identity) => received.push(`chunk:${identity.projectSlug}:${identity.token}:${data}`)
    }
  });

  controller.start();
  sockets[0].open();
  sockets[0].message({ type: "snapshot", data: "old" });
  sockets[0].message({ type: "chunk", data: "live-a" });
  sockets[0].message({ type: "snapshot" });
  controller.configure("beta", "beta-shell-1", "token-b");
  sockets[0].message({ type: "snapshot", data: "stale" });
  sockets[1].open();
  sockets[1].message({ type: "snapshot", data: "fresh" });
  sockets[1].message({ type: "chunk", data: "live-b" });
  controller.stop();

  const expected = [
    "snapshot:alpha:token-a:old",
    "chunk:alpha:token-a:live-a",
    "snapshot:alpha:token-a:",
    "snapshot:beta:token-b:fresh",
    "chunk:beta:token-b:live-b"
  ];
  if (JSON.stringify(received) !== JSON.stringify(expected)) {
    throw new Error(`terminal stream events = ${JSON.stringify(received)}, want ${JSON.stringify(expected)}`);
  }
}

probeSnapshotBoundaryAndStaleIdentity();
