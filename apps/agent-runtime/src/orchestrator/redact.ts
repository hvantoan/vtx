/**
 * Secret redaction for evidence before it is persisted or synced to the
 * dashboard (design doc §7, NFR-AUD-001 "no secrets"). Applied on the
 * `tool_result` hook to both the model-facing `content` and the machine-readable
 * `structuredContent`.
 *
 * Redaction is deliberately aggressive and value-agnostic: it masks known token
 * shapes and any caller-supplied secret values, and never attempts to judge
 * whether a string "looks sensitive enough" to keep.
 */

/** Shapes that are masked wherever they appear in a string. */
const SECRET_PATTERNS: RegExp[] = [
  /\bgh[pousr]_[A-Za-z0-9]{20,}\b/g, // GitHub PAT / OAuth / refresh tokens
  /\bgithub_pat_[A-Za-z0-9_]{20,}\b/g,
  /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/g, // Slack tokens
  /\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/g, // JWT
  /\b[A-Za-z0-9_-]*token[A-Za-z0-9_-]*\s*[:=]\s*\S+/gi, // token=... / api_token: ...
  /\b[A-Za-z0-9_-]*(secret|password|passwd|api[_-]?key)[A-Za-z0-9_-]*\s*[:=]\s*\S+/gi,
  /\bBearer\s+[A-Za-z0-9._-]{16,}\b/gi,
  /-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/g,
];

const MASK = "[REDACTED]";

/** Redact secret shapes and caller-supplied values from a single string. */
export function redactText(input: string, secretValues: readonly string[] = []): string {
  let out = input;
  for (const pattern of SECRET_PATTERNS) {
    pattern.lastIndex = 0;
    out = out.replace(pattern, MASK);
  }
  for (const secret of secretValues.filter((s) => s.length > 0).sort((a, b) => b.length - a.length)) {
    out = out.split(secret).join(MASK);
  }
  return out;
}

/**
 * Deep-redact an arbitrary JSON value. Strings are pattern-masked; objects and
 * arrays are walked. Any key whose name looks secret has its value fully masked
 * regardless of shape, so a structured `{ api_token: "..." }` cannot leak.
 */
export function redactValue(value: unknown, secretValues: readonly string[] = []): unknown {
  const isSecretKey = (key: string): boolean =>
    /(token|secret|password|passwd|api[_-]?key|authorization|credential)/i.test(key);

  const walk = (v: unknown, key?: string): unknown => {
    // A secret-looking key masks its value regardless of type, so a structured
    // `{ api_token: 123 }` cannot leak a non-string credential.
    if (key && isSecretKey(key)) return MASK;
    if (typeof v === "string") {
      return redactText(v, secretValues);
    }
    if (Array.isArray(v)) return v.map((item) => walk(item));
    if (v && typeof v === "object") {
      const out: Record<string, unknown> = {};
      for (const [k, item] of Object.entries(v as Record<string, unknown>)) {
        out[k] = walk(item, k);
      }
      return out;
    }
    return v;
  };

  return walk(value);
}
