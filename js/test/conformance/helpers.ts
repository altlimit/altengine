import { AltEngine } from "../../src/index.js";

export const baseUrl = (): string => {
  const url = process.env.ALTENGINE_TEST_URL;
  if (!url) throw new Error("conformance setup did not run (ALTENGINE_TEST_URL unset)");
  return url;
};

export const apiKey = (): string => process.env.ALTENGINE_TEST_KEY || "conformance-dev-key";

export const isEmulator = (): boolean => !process.env.ALTENGINE_CONFORMANCE_URL;

export const destructiveOk = (): boolean =>
  isEmulator() || process.env.ALTENGINE_CONFORMANCE_DESTRUCTIVE === "1";

export const client = (): AltEngine => new AltEngine({ baseUrl: baseUrl(), apiKey: apiKey() });

/** Random suffix so suites never collide across runs or languages. */
export const uniq = (prefix: string): string =>
  `${prefix}-${Date.now().toString(36)}${Math.random().toString(36).slice(2, 7)}`;
