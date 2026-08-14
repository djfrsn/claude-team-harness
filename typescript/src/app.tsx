import type { VNode } from "preact";

interface QualityCheck {
  readonly code: string;
  readonly label: string;
}

const qualityChecks: readonly QualityCheck[] = [
  { code: "01", label: "Strict types" },
  { code: "02", label: "Static analysis" },
  { code: "03", label: "Coverage floor" },
  { code: "04", label: "Dependency hygiene" },
];

export function App(): VNode {
  return (
    <main class="app-shell">
      <header class="masthead">
        <a class="wordmark" href="/" aria-label="Claude Team Harness home">
          <span>CTH</span>
          <span aria-hidden="true">/</span>
          <span>00</span>
        </a>
        <span class="system-state" role="status">
          <i aria-hidden="true" />
          Starter online
        </span>
      </header>

      <section class="hero" aria-labelledby="hello-title">
        <div class="hero-copy">
          <p class="eyebrow">Claude Team Harness · TypeScript starter</p>
          <h1 id="hello-title">
            Hello,
            <span>team.</span>
          </h1>
          <p class="intro">
            A small Preact surface with the same strict TypeScript floor used by
            Studio. Ready for the first interface that earns its place here.
          </p>
        </div>
        <div class="signal" aria-hidden="true">
          <span>AGENT / READY</span>
          <div class="signal-orbit">
            <i />
          </div>
          <span>SESSION / 001</span>
        </div>
      </section>

      <section class="quality" aria-labelledby="quality-title">
        <header>
          <div>
            <p class="section-index">SYSTEM FLOOR</p>
            <h2 id="quality-title">Quality circuit</h2>
          </div>
          <span>{qualityChecks.length} checks armed</span>
        </header>
        <ol>
          {qualityChecks.map((check) => (
            <li key={check.code}>
              <span>{check.code}</span>
              <strong>{check.label}</strong>
              <i aria-hidden="true" />
            </li>
          ))}
        </ol>
      </section>

      <footer>
        <span>Preact · TypeScript · esbuild · Vitest</span>
        <code>pnpm check</code>
      </footer>
    </main>
  );
}
