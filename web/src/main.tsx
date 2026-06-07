import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

function App() {
  return (
    <main className="app-shell">
      <header className="topbar">
        <div>
          <h1>ccx-t2</h1>
          <p>Task ledger, workers, and orchestration controls</p>
        </div>
      </header>
      <section className="workspace">
        <div className="panel">
          <h2>Task Ledger</h2>
          <p>Frontend scaffold is ready for the Phase 5 task views.</p>
        </div>
        <div className="panel">
          <h2>Worker Dashboard</h2>
          <p>WebSocket proxy routes are configured for worker logs and ledger updates.</p>
        </div>
      </section>
    </main>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>
);
