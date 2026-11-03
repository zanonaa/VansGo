// Pure helpers for the per-combo live test feature (POST /api/combos/{id}/test).
// Kept free of React/DOM so the dashboard can unit-test the contract.

const ATTEMPT_TEXT_LIMIT = 400;

const toInt = (value, fallback = 0) => {
  const n = typeof value === "string" ? Number(value) : value;
  return Number.isFinite(n) ? Math.trunc(n) : fallback;
};

const toText = (value) => (typeof value === "string" ? value : value == null ? "" : String(value));

const truncate = (text, limit) =>
  text.length <= limit ? text : `${text.slice(0, limit)}…`;

// Normaliza a resposta do endpoint para o modal. Tolerante a campos ausentes
// (o gateway evolui esse contrato) — nada explode.
export function normalizeComboTestResponse(raw) {
  if (!raw || typeof raw !== "object") {
    return {
      ok: false,
      strategy: "",
      combo: "",
      models: [],
      attempts: [],
      stoppedBy: false,
      error: "Empty response from combo test",
      durationMs: 0,
      testedAt: "",
    };
  }
  const attempts = Array.isArray(raw.attempts)
    ? raw.attempts.map((entry) => {
        const source = entry && typeof entry === "object" ? entry : {};
        const error = truncate(toText(source.error), ATTEMPT_TEXT_LIMIT);
        return {
          model: toText(source.model),
          ok: source.ok === true,
          status: toInt(source.status, 0),
          latencyMs: Math.max(0, toInt(source.latencyMs, 0)),
          error,
        };
      })
    : [];
  return {
    ok: raw.ok === true,
    strategy: toText(raw.strategy),
    combo: toText(raw.combo),
    models: Array.isArray(raw.models) ? raw.models.map(toText) : [],
    attempts,
    stoppedBy: raw.stoppedBy === true,
    error: truncate(toText(raw.error), ATTEMPT_TEXT_LIMIT),
    durationMs: toInt(raw.durationMs, 0),
    testedAt: toText(raw.testedAt),
  };
}

// Texto de 1 linha por tentativa para o relatório copiável.
export function describeAttempt(attempt) {
  const verdict = attempt.ok ? "OK" : "FAIL";
  const status = attempt.status > 0 ? `HTTP ${attempt.status}` : "no response";
  const latency = attempt.latencyMs > 0 ? `${attempt.latencyMs}ms` : "—";
  const suffix = attempt.error ? `: ${attempt.error}` : "";
  return `${verdict} ${attempt.model} (${status}, ${latency}${suffix})`;
}

// Relatório multilinha para a área de transferência.
export function buildComboTestReport({ combo, result }) {
  const name = toText(combo?.name || result?.combo) || "combo";
  const lines = [
    `Combo test report — ${name}`,
    `Strategy: ${result?.strategy || "unknown"} · ok: ${result?.ok ? "true" : "false"} · duration: ${toInt(result?.durationMs, 0)}ms`,
    result?.testedAt ? `Tested at: ${result.testedAt}` : null,
    "",
  ].filter(Boolean);
  if (Array.isArray(result?.models) && result.models.length > 0) {
    lines.push(`Chain: ${result.models.join(" → ")}`, "");
  }
  for (const attempt of result?.attempts ?? []) {
    lines.push(`- ${describeAttempt(attempt)}`);
  }
  if (result?.stoppedBy) {
    lines.push("", "Stopped: first non-retryable error (strategy parity with account fallback).");
  }
  if (result?.error) {
    lines.push("", `Error: ${result.error}`);
  }
  return lines.join("\n");
}
