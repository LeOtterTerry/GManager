const views = ["status", "config", "logs", "jobs"] as const;
type ViewName = (typeof views)[number];

function isView(value: string): value is ViewName {
  return views.some((view) => view === value);
}

function showView(name: ViewName, moveFocus: boolean): void {
  for (const view of views) {
    const panel = document.getElementById(`view-${view}`);
    const button = document.querySelector<HTMLButtonElement>(`button[data-view="${view}"]`);
    if (panel === null || button === null) {
      return;
    }
    const active = view === name;
    panel.hidden = !active;
    if (active) {
      button.setAttribute("aria-current", "page");
    } else {
      button.removeAttribute("aria-current");
    }
  }
  if (!moveFocus) {
    return;
  }
  document.getElementById(`${name}-heading`)?.focus();
}

function bindNav(): void {
  for (const view of views) {
    const button = document.querySelector<HTMLButtonElement>(`button[data-view="${view}"]`);
    button?.addEventListener("click", () => {
      showView(view, true);
    });
  }
}

interface Health {
  status: string;
  bindHost?: string;
}

function isHealth(value: unknown): value is Health {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  return typeof (value as { status?: unknown }).status === "string";
}

async function errorMessage(response: Response): Promise<string> {
  try {
    const payload: unknown = await response.json();
    if (typeof payload === "object" && payload !== null) {
      const message = (payload as { message?: unknown }).message;
      if (typeof message === "string" && message !== "") {
        return message;
      }
    }
  } catch {
    return response.statusText || "The agent returned an error.";
  }
  return response.statusText || "The agent returned an error.";
}

function setFact(id: string, text: string): void {
  const node = document.getElementById(id);
  if (node !== null) {
    node.textContent = text;
  }
}

async function checkAgent(token: string): Promise<void> {
  const result = document.getElementById("agent-result");
  if (token.trim() === "") {
    setFact("fact-reachable", "Not checked");
    setFact("fact-bind", "—");
    if (result !== null) {
      result.textContent = "Enter the agent token.";
    }
    return;
  }
  if (result !== null) {
    result.textContent = "";
  }
  try {
    const response = await fetch("/api/v1/health", {
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(3000),
    });
    if (response.status === 401) {
      setFact("fact-reachable", "Rejected");
      setFact("fact-bind", "—");
      if (result !== null) {
        result.textContent = "The agent rejected that token.";
      }
      return;
    }
    if (!response.ok) {
      setFact("fact-reachable", "Error");
      setFact("fact-bind", "—");
      if (result !== null) {
        result.textContent = await errorMessage(response);
      }
      return;
    }
    const payload: unknown = await response.json();
    if (!isHealth(payload)) {
      setFact("fact-reachable", "Error");
      setFact("fact-bind", "—");
      if (result !== null) {
        result.textContent = "The agent returned an unexpected health response.";
      }
      return;
    }
    setFact("fact-reachable", "Yes");
    setFact("fact-bind", payload.bindHost ?? "—");
    if (result !== null) {
      result.textContent = "Agent is up.";
    }
  } catch {
    setFact("fact-reachable", "No");
    setFact("fact-bind", "—");
    if (result !== null) {
      result.textContent = "Agent is not reachable at /api/v1/health.";
    }
  }
}

function bindCheck(): void {
  const form = document.querySelector<HTMLFormElement>("#agent-check");
  const input = document.querySelector<HTMLInputElement>("#agent-token");
  form?.addEventListener("submit", (event) => {
    event.preventDefault();
    void checkAgent(input?.value ?? "");
  });
}

const initial = "status";
if (isView(initial)) {
  showView(initial, false);
}
bindNav();
bindCheck();
