// tabFromHash picks the tab named by a URL hash ("#sessions"), falling
// back to the first tab for an empty or unknown hash.
export function tabFromHash(hash: string, tabs: string[]): string {
  const want = hash.replace(/^#/, "");
  return tabs.includes(want) ? want : tabs[0];
}
