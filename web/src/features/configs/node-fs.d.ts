// The one Node call a test here makes (vars.test.ts reads the cases it
// shares with the server); the project carries no Node typings.
declare module "node:fs" {
  export function readFileSync(path: URL | string, encoding: "utf8"): string;
}
