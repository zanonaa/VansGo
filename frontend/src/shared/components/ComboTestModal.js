"use client";

import Modal from "./Modal";
import Button from "./Button";
import { useCopyToClipboard } from "@/shared/hooks/useCopyToClipboard";
import { buildComboTestReport } from "@/shared/comboTest";

// ComboTestModal — exibe o resultado de POST /api/combos/{id}/test:
// veredito, cadeia resolvida e cada tentativa com status/latência/erro real.
export default function ComboTestModal({ isOpen, onClose, combo, result, error, loading }) {
  const { copied, copy } = useCopyToClipboard();
  const attempts = Array.isArray(result?.attempts) ? result.attempts : [];
  const passed = result?.ok === true;
  const chain = Array.isArray(result?.models) && result.models.length > 0 ? result.models.join(" → ") : "";
  const report = result ? buildComboTestReport({ combo, result }) : "";
  const copyKey = `combo-test-${combo?.id || "unknown"}`;

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      title={combo ? `Test — ${combo.name}` : "Combo test"}
      size="lg"
      footer={
        <>
          <Button variant="ghost" icon={copied === copyKey ? "check" : "content_copy"} onClick={() => copy(report, copyKey)} disabled={!result}>
            {copied === copyKey ? "Copied" : "Copy report"}
          </Button>
          <Button variant="primary" onClick={onClose}>Close</Button>
        </>
      }
    >
      {loading && (
        <div className="flex flex-col items-center justify-center gap-3 py-10 text-center">
          <span className="material-symbols-outlined animate-spin text-[36px] text-primary">progress_activity</span>
          <p className="text-sm font-medium text-text-main">Running live test…</p>
          <p className="max-w-sm text-xs text-text-muted">
            Sends a minimal “hi” request to each candidate in strategy order. This may consume a few tokens.
          </p>
        </div>
      )}

      {!loading && result && (
        <div className="flex flex-col gap-4">
          {/* Verdict banner */}
          <div
            className={`flex items-start gap-2 rounded-lg border px-3 py-2.5 ${
              passed
                ? "border-emerald-500/30 bg-emerald-500/[0.07] text-emerald-700 dark:text-emerald-400"
                : "border-red-500/30 bg-red-500/[0.07] text-red-600 dark:text-red-400"
            }`}
            role="status"
          >
            <span className="material-symbols-outlined mt-0.5 text-[20px]">
              {passed ? "check_circle" : "error"}
            </span>
            <div className="min-w-0">
              <p className="text-sm font-semibold">
                {passed ? "Combo responded successfully" : "Combo test failed"}
              </p>
              <p className="text-xs opacity-80">
                Strategy <code className="font-mono">{result.strategy || "unknown"}</code>
                {typeof result.durationMs === "number" ? ` · ${result.durationMs}ms` : ""}
                {result.stoppedBy ? " · stopped at upstream content block" : ""}
              </p>
            </div>
          </div>

          {/* Resolved chain */}
          {chain && (
            <div>
              <p className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-text-muted">
                Resolved chain
              </p>
              <p className="break-words rounded-lg bg-surface-2/60 px-3 py-2 font-mono text-xs text-text-main">
                {chain}
              </p>
            </div>
          )}

          {/* Attempts */}
          {attempts.length > 0 && (
            <div>
              <p className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-text-muted">
                Attempts ({attempts.length})
              </p>
              <ul className="flex flex-col gap-1.5">
                {attempts.map((attempt, index) => (
                  <li
                    key={`${attempt.model}-${index}`}
                    className="flex flex-col gap-1 rounded-lg border border-border-subtle px-3 py-2"
                  >
                    <div className="flex min-w-0 items-center gap-2">
                      <span
                        className={`material-symbols-outlined shrink-0 text-[18px] ${
                          attempt.ok ? "text-emerald-600 dark:text-emerald-400" : "text-red-500 dark:text-red-400"
                        }`}
                      >
                        {attempt.ok ? "check_circle" : "cancel"}
                      </span>
                      <code className="min-w-0 flex-1 truncate font-mono text-xs text-text-main" title={attempt.model}>
                        {attempt.model}
                      </code>
                      {attempt.status > 0 && (
                        <span
                          className={`shrink-0 rounded px-1.5 py-0.5 font-mono text-[10px] font-semibold ${
                            attempt.ok
                              ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-400"
                              : "bg-red-500/10 text-red-600 dark:text-red-400"
                          }`}
                        >
                          HTTP {attempt.status}
                        </span>
                      )}
                      {attempt.latencyMs > 0 && (
                        <span className="shrink-0 font-mono text-[10px] text-text-muted">
                          {attempt.latencyMs}ms
                        </span>
                      )}
                    </div>
                    {attempt.error && (
                      <p className="break-words pl-7 font-mono text-[11px] leading-snug text-red-600 dark:text-red-400">
                        {attempt.error}
                      </p>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {/* Unified error line (request-level / expansion failure) */}
          {error && (
            <p className="rounded-lg border border-red-500/30 bg-red-500/[0.07] px-3 py-2 font-mono text-xs text-red-600 dark:text-red-400">
              {error}
            </p>
          )}
        </div>
      )}

      {!loading && !result && (
        <div className="flex flex-col items-center gap-2 py-8 text-center">
          <span className="material-symbols-outlined text-[36px] text-red-500">error</span>
          <p className="text-sm font-medium text-text-main">Combo test did not run</p>
          <p className="break-words font-mono text-xs text-red-600 dark:text-red-400">{error || "Unknown error"}</p>
        </div>
      )}
    </Modal>
  );
}
