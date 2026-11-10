// Minimal typings for asciinema-player v3 (the package ships none).
declare module "asciinema-player" {
  export interface Player {
    play(): Promise<void>;
    pause(): void;
    seek(location: number | string): Promise<void>;
    getCurrentTime(): number;
    dispose(): void;
  }
  /** A custom driver: the player renders what it dispatches ("reset",
   *  "output", "resize") and routes its keyboard shortcuts to it. */
  export type Driver = (cb: { dispatch(event: string, data?: unknown): void }) => {
    play?(): void;
    pause?(): void;
    seek?(where: unknown): boolean;
    getTiming(): { time: number; rate: number };
    stop?(): void;
  };
  export function create(src: string | { url: string } | Driver, element: HTMLElement, opts?: Record<string, unknown>): Player;
}
