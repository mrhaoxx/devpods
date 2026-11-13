// A text box that keeps what is typed and hands it on once typing pauses:
// each value handed on may refilter a thousand rows or ask the server, so
// one a key lags. A value changed elsewhere (back, a link) shows in the box.
import { useEffect, useRef, useState } from "react";

export function useDebounced(value: string, onChange: (v: string) => void, ms = 200): [string, (v: string) => void] {
  const [text, setText] = useState(value);
  const sent = useRef(value);
  const send = useRef(onChange);
  send.current = onChange;
  useEffect(() => {
    if (value !== sent.current) {
      sent.current = value;
      setText(value);
    }
  }, [value]);
  useEffect(() => {
    if (text === sent.current) return;
    const t = setTimeout(() => {
      sent.current = text;
      send.current(text);
    }, ms);
    return () => clearTimeout(t);
  }, [text, ms]);
  return [text, setText];
}
