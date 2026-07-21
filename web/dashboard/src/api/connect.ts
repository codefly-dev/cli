// Minimal Connect protocol client for the codefly.cli.v0.CLI service.
//
// The CLI exposes its API over the Connect protocol (connect-go). Rather than
// pull in a proto codegen toolchain, we speak Connect directly over fetch: it
// is a small, stable wire format and the CLI surface is narrow. Request and
// response bodies are proto3 JSON (camelCase field names, enums as strings,
// timestamps as RFC 3339).

const SERVICE = "/codefly.cli.v0.CLI";

export class ConnectError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "ConnectError";
    this.code = code;
  }
}

export async function unary<Res>(
  method: string,
  request: unknown,
  signal?: AbortSignal,
): Promise<Res> {
  const res = await fetch(`${SERVICE}/${method}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
    },
    body: JSON.stringify(request ?? {}),
    signal,
  });
  if (!res.ok) {
    let code = `http_${res.status}`;
    let message = res.statusText;
    try {
      const body = await res.json();
      if (typeof body.code === "string") code = body.code;
      if (typeof body.message === "string") message = body.message;
    } catch {
      // response body was not Connect error JSON; keep the HTTP status
    }
    throw new ConnectError(code, message);
  }
  return (await res.json()) as Res;
}

// serverStream consumes a Connect server-streaming response. Each message is an
// enveloped frame: a 5-byte header (1 flag byte + big-endian uint32 length)
// followed by the JSON payload. The end-of-stream frame has the 0b10 flag set
// and carries trailing metadata or an error.
export async function* serverStream<Res>(
  method: string,
  request: unknown,
  signal?: AbortSignal,
): AsyncGenerator<Res> {
  const res = await fetch(`${SERVICE}/${method}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/connect+json",
      "Connect-Protocol-Version": "1",
    },
    body: encodeEnvelope(request ?? {}),
    signal,
  });
  if (!res.ok || !res.body) {
    throw new ConnectError(`http_${res.status}`, res.statusText);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer: Uint8Array = new Uint8Array(0);
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer = concat(buffer, value);
    while (buffer.length >= 5) {
      const view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);
      const flags = view.getUint8(0);
      const length = view.getUint32(1);
      if (buffer.length < 5 + length) break;
      const payload = buffer.subarray(5, 5 + length);
      buffer = buffer.slice(5 + length);
      const text = decoder.decode(payload);
      if (flags & 0b10) {
        const end = text ? JSON.parse(text) : {};
        if (end.error) {
          throw new ConnectError(end.error.code ?? "unknown", end.error.message ?? "stream error");
        }
        return;
      }
      yield JSON.parse(text) as Res;
    }
  }
}

function encodeEnvelope(request: unknown): Uint8Array<ArrayBuffer> {
  const payload = new TextEncoder().encode(JSON.stringify(request));
  const frame = new Uint8Array(new ArrayBuffer(5 + payload.length));
  new DataView(frame.buffer).setUint32(1, payload.length);
  frame.set(payload, 5);
  return frame;
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}
