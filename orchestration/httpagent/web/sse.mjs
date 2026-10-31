// SSE 按协议行解析；网络分片由 readEvents 的流式 UTF-8 解码处理。
export class SSEParser {
  constructor(onEvent) {
    this.onEvent = onEvent;
    this.line = "";
    this.data = [];
    this.kind = "message";
    this.cr = false;
  }
  push(text) {
    for (const char of text) {
      if (this.cr) {
        this.cr = false;
        if (char === "\n") continue;
      }
      if (char === "\r" || char === "\n") {
        this.consume(this.line);
        this.line = "";
        this.cr = char === "\r";
      } else this.line += char;
    }
  }
  consume(line) {
    if (line === "") {
      if (this.data.length) {
        const event = JSON.parse(this.data.join("\n"));
        this.onEvent(event, this.kind);
      }
      this.data = [];
      this.kind = "message";
      return;
    }
    if (line.startsWith(":")) return;
    const colon = line.indexOf(":");
    const key = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (key === "data") this.data.push(value);
    if (key === "event") this.kind = value;
  }
}
export async function readEvents(body, onEvent) {
  if (!body) throw new Error("响应没有事件流");
  let terminal = false;
  const parser = new SSEParser((event) => {
    if (terminal) throw new Error("终态后收到额外事件");
    if (event.type === "done" || event.type === "error") terminal = true;
    onEvent(event);
  });
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      parser.push(decoder.decode(value, { stream: true }));
    }
    parser.push(decoder.decode());
    if (!terminal) throw new Error("连接结束，未确认完成；请勿自动重新提交");
  } finally {
    reader.releaseLock();
  }
}
