import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { api, ApiError, shellSocketURL, type Instance } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

type Status =
  | { kind: "picking" }
  | { kind: "connecting"; instance: string }
  | { kind: "open"; instance: string; shell: string }
  | { kind: "closed"; instance: string; message: string };

/** Control frames from the server; binary frames are terminal bytes. */
type Control =
  | { type: "open"; instance: string; shell: string }
  | { type: "exit"; code: number }
  | { type: "error"; message: string };

/** A shell into a running instance (RFC-0026): xterm.js over a WebSocket the
 * server bridges to pods/exec. */
export function ShellView({ slug }: { slug: string }) {
  const instances = useQuery({
    queryKey: ["instances", slug],
    queryFn: () => api.instances(slug),
    refetchInterval: 15_000,
  });
  const [instance, setInstance] = useState<string>("");
  const [status, setStatus] = useState<Status>({ kind: "picking" });
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const ws = useRef<WebSocket | null>(null);
  // xterm's onData returns a disposable. Reconnecting without disposing the
  // previous one leaves a handler per session attached to the same terminal.
  const onData = useRef<{ dispose(): void } | null>(null);

  // Default to the first ready instance once the list arrives.
  useEffect(() => {
    if (!instance && instances.data?.length) {
      setInstance((instances.data.find((i) => i.ready) ?? instances.data[0]).name);
    }
  }, [instances.data, instance]);

  // One terminal for the life of the tab; sessions come and go inside it.
  useEffect(() => {
    if (!host.current) return;
    const t = new Terminal({
      convertEol: false,
      cursorBlink: true,
      fontFamily:
        'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: { background: "#09090b", foreground: "#f4f4f5" },
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(host.current);
    f.fit();
    term.current = t;
    fit.current = f;
    return () => {
      t.dispose();
      term.current = null;
      fit.current = null;
    };
  }, []);

  const sendResize = useCallback(() => {
    const t = term.current;
    const sock = ws.current;
    fit.current?.fit();
    if (!t || sock?.readyState !== WebSocket.OPEN) return;
    sock.send(
      JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }),
    );
  }, []);

  // Refit on container resize, debounced: dragging a window edge otherwise
  // sends a frame per pixel.
  useEffect(() => {
    if (!host.current) return;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const ro = new ResizeObserver(() => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(sendResize, 100);
    });
    ro.observe(host.current);
    return () => {
      ro.disconnect();
      if (timer) clearTimeout(timer);
    };
  }, [sendResize]);

  const connect = useCallback(async () => {
    const t = term.current;
    if (!t || !instance) return;
    ws.current?.close();
    t.reset();
    setStatus({ kind: "connecting", instance });
    t.writeln(`Connecting to ${instance}...`);
    let ticket: string;
    try {
      ticket = (await api.shellTicket(slug, instance)).ticket;
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : String(e);
      t.writeln(`\r\n${msg}`);
      setStatus({ kind: "closed", instance, message: msg });
      return;
    }
    const sock = new WebSocket(shellSocketURL(slug, instance, ticket));
    sock.binaryType = "arraybuffer";
    ws.current = sock;

    sock.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        let ctl: Control;
        try {
          ctl = JSON.parse(ev.data) as Control;
        } catch {
          return;
        }
        if (ctl.type === "open") {
          setStatus({ kind: "open", instance: ctl.instance, shell: ctl.shell });
          sendResize();
        } else if (ctl.type === "exit") {
          setStatus({
            kind: "closed",
            instance,
            message: `Process exited with status ${ctl.code}`,
          });
        } else if (ctl.type === "error") {
          t.writeln(`\r\n${ctl.message}`);
          setStatus({ kind: "closed", instance, message: ctl.message });
        }
        return;
      }
      // Bytes, not text: a Uint8Array keeps output a program prints intact,
      // and xterm decodes UTF-8 across chunk boundaries itself.
      t.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    sock.onclose = () => {
      if (ws.current === sock) ws.current = null;
      setStatus((prev) =>
        prev.kind === "closed"
          ? prev
          : { kind: "closed", instance, message: "Session ended" },
      );
    };
    sock.onerror = () => {
      t.writeln("\r\nThe connection failed.");
    };
    // Keystrokes go out as bytes for the same reason output comes back as
    // bytes.
    const enc = new TextEncoder();
    onData.current?.dispose();
    onData.current = t.onData((d) => {
      if (sock.readyState === WebSocket.OPEN) sock.send(enc.encode(d));
    });
  }, [instance, sendResize, slug]);

  // Closing the socket on unmount is what ends the session when the tab is
  // closed or the user navigates away.
  useEffect(
    () => () => {
      onData.current?.dispose();
      ws.current?.close();
    },
    [],
  );

  const live = status.kind === "open" || status.kind === "connecting";
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={instance} onValueChange={setInstance} disabled={live}>
          <SelectTrigger className="w-56">
            <SelectValue placeholder="Choose an instance" />
          </SelectTrigger>
          <SelectContent>
            {(instances.data ?? []).map((i: Instance) => (
              <SelectItem key={i.name} value={i.name}>
                {i.name}
                {i.ready ? "" : " (not ready)"}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {live ? (
          <Button variant="outline" onClick={() => ws.current?.close()}>
            Disconnect
          </Button>
        ) : (
          <Button onClick={connect} disabled={!instance}>
            {status.kind === "closed" ? "Reconnect" : "Connect"}
          </Button>
        )}
        <span className="text-xs text-muted-foreground">
          {status.kind === "open"
            ? `${status.instance} · ${status.shell}`
            : status.kind === "closed"
              ? status.message
              : status.kind === "connecting"
                ? `Connecting to ${status.instance}...`
                : (instances.data?.length ?? 0) === 0
                  ? "No running instances"
                  : "One shell per project at a time; idle sessions close after 30 minutes."}
        </span>
      </div>
      <div
        ref={host}
        className="h-[30rem] overflow-hidden rounded-lg border bg-zinc-950 p-2"
      />
    </div>
  );
}
