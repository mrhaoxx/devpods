import { LiveHub } from "../data/live/LiveHub";
import { openSSE } from "../data/sse";

/** The app's one LiveHub. */
export const appHub = new LiveHub({ open: openSSE });
