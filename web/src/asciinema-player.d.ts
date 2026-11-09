// Minimal typings for asciinema-player v3 (the package ships none).
declare module "asciinema-player" {
  export interface Player {
    play(): Promise<void>;
    pause(): void;
    seek(location: number | string): Promise<void>;
    getCurrentTime(): number;
    dispose(): void;
  }
  export function create(src: string | { url: string }, element: HTMLElement, opts?: Record<string, unknown>): Player;
}
