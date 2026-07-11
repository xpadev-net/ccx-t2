import { useEffect, useRef, useSyncExternalStore } from "react";
import {
  TerminalController,
  type TerminalControllerCallbacks,
  type TerminalControllerOptions,
  type TerminalSize,
  type TerminalState
} from "./controller";

export type UseTerminalControllerOptions = Omit<
  TerminalControllerOptions,
  "projectSlug" | "windowName" | "callbacks"
> & {
  projectSlug: string;
  windowName: string;
  onStateChange?: TerminalControllerCallbacks["onStateChange"];
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

export function useTerminalController(options: UseTerminalControllerOptions): UseTerminalControllerResult {
  const controllerRef = useRef<TerminalController | null>(null);
  if (controllerRef.current === null) {
    controllerRef.current = new TerminalController({
      ...options,
      callbacks: { onStateChange: options.onStateChange, onData: options.onData, onError: options.onError }
    });
  }
  const controller = controllerRef.current;

  useEffect(() => {
    controller.setCallbacks({
      onStateChange: options.onStateChange,
      onData: options.onData,
      onError: options.onError
    });
    return () => controller.setCallbacks({});
  }, [controller, options.onData, options.onError, options.onStateChange]);

  useEffect(() => {
    controller.configure(options.projectSlug, options.windowName, options.token ?? "");
  }, [controller, options.projectSlug, options.token, options.windowName]);

  useEffect(() => {
    if (!options.autoStart && options.autoStart !== undefined) {
      return;
    }
    controller.start();
    return () => controller.stop();
  }, [controller, options.autoStart]);

  const state = useSyncExternalStore(
    (listener) => controller.subscribe(listener),
    () => controller.getSnapshot(),
    () => controller.getSnapshot()
  );
  const generation = controller.getGeneration();

  return {
    controller,
    state,
    generation,
    retry: () => controller.retry(),
    sendInput: (data) => controller.sendInput(data, generation),
    resize: (size) => controller.resize(size.cols, size.rows, generation)
  };
}
