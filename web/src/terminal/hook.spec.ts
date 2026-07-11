import { matchesTerminalConnection } from "./hook";

const target = { projectSlug: "beta", windowName: "beta-shell-1" };
const fresh = { ...target, generation: 8, token: "token-b" };

if (!matchesTerminalConnection(fresh, 8, target, "token-b")) {
  throw new Error("fresh post-options connection identity was rejected");
}
if (matchesTerminalConnection({ ...fresh, generation: 7 }, 8, target, "token-b")) {
  throw new Error("stale generation was accepted");
}
if (matchesTerminalConnection({ ...fresh, projectSlug: "alpha" }, 8, target, "token-b")) {
  throw new Error("stale target was accepted");
}
if (matchesTerminalConnection({ ...fresh, token: "token-a" }, 8, target, "token-b")) {
  throw new Error("stale credential was accepted");
}
