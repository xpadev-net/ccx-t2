import { useEffect, useRef, useSyncExternalStore } from "react";
import {
  TerminalController,
  type TerminalConnectionIdentity,
  type TerminalControllerCallbacks,
  type TerminalControllerOptions,
  type TerminalSize,
  type TerminalState,
  type TerminalTarget
} from "./controller";

export type UseTerminalControllerOptions = Omit<
  TerminalControllerOptions,
  "projectSlug" | "windowName" | "callbacks"
> & {
  projectSlug: string;
  windowName: string;
  onStateChange?: TerminalControllerCallbacks["onStateChange"];
  onSnapshot?: TerminalControllerCallbacks["onSnapshot"];
  onData?: TerminalControllerCallbacks["onData"];
  onError?: TerminalControllerCallbacks["onError"];
  autoStart?: boolean;
};

export type UseTerminalControllerResult = {
  controller: TerminalController;
  state: TerminalState;
  generation: number;
  retry: () => void;
  sendInput: (data: string) => boolean;
  resize: (size: TerminalSize) => boolean;
};

export function matchesTerminalConnection(
  identity: TerminalConnectionIdentity,
  generation: number,
  target: TerminalTarget,
  token: string
): boolean {
  return identity.generation === generation && identity.projectSlug === target.projectSlug &&
    identity.windowName === target.windowName && identity.token === token;
}

export function useTerminalController(options: UseTerminalControllerOptions): UseTerminalControllerResult {
  const controllerRef = useRef<TerminalController | null>(null);
  if (controllerRef.current === null) {
    const {
      projectSlug,
      windowName,
      token,
      baseUrl,
      fetch,
      websocketFactory,
      location,
      timers,
      random,
      retry,
      stableOpenMs,
      probeTimeoutMs,
      wakeTarget,
      visibilityTarget,
      isVisible
    } = options;
    controllerRef.current = new TerminalController({
      projectSlug,
      windowName,
      token,
      baseUrl,
      fetch,
      websocketFactory,
      location,
      timers,
      random,
      retry,
      stableOpenMs,
      probeTimeoutMs,
      wakeTarget,
      visibilityTarget,
      isVisible,
      callbacks: { onStateChange: options.onStateChange, onError: options.onError }
    });
  }
  const controller = controllerRef.current;
  const generation = controller.getGeneration();
  const target: TerminalTarget = { projectSlug: options.projectSlug, windowName: options.windowName };
  const token = options.token ?? "";

  useEffect(() => {
    const accepts = (identity: TerminalConnectionIdentity) => matchesTerminalConnection(
      identity,
      controller.getGeneration(),
      target,
      token
    );
    controller.setCallbacks({
      onStateChange: options.onStateChange,
      onSnapshot: (data, identity) => { if (accepts(identity)) options.onSnapshot?.(data, identity); },
      onData: (data, identity) => { if (accepts(identity)) options.onData?.(data, identity); },
      onError: options.onError
    });
    return () => controller.setCallbacks({});
  }, [controller, generation, options.onData, options.onError, options.onSnapshot, options.onStateChange,
    target.projectSlug, target.windowName, token]);

  useEffect(() => {
    controller.updateOptions(options);
  }, [
    controller,
    options.projectSlug,
    options.windowName,
    options.token,
    options.baseUrl,
    options.fetch,
    options.websocketFactory,
    options.location?.protocol,
    options.location?.host,
    options.timers,
    options.random,
    options.retry?.baseMs,
    options.retry?.maxMs,
    options.retry?.jitterRatio,
    options.stableOpenMs,
    options.probeTimeoutMs,
    options.wakeTarget,
    options.visibilityTarget,
    options.isVisible
  ]);

  useEffect(() => {
    if (!options.autoStart && options.autoStart !== undefined) {
      return;
    }
    controller.start();
    return () => controller.stop();
  }, [controller, options.autoStart]);

  const state = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot
  );
  // Keep event handlers bound to the render's project/window target and generation so
  // queued callbacks fail closed both before and after a passive target-switch effect.

  return {
    controller,
    state,
    generation,
    retry: () => controller.retry(),
    sendInput: (data) => controller.sendInput(data, generation, target),
    resize: (size) => controller.resize(size.cols, size.rows, generation, target)
  };
}
