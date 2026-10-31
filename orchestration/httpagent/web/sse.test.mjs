import test from "node:test";
import assert from "node:assert/strict";
import { SSEParser, readEvents } from "./sse.mjs";

test("跨UTF8网络分片、CRLF和多data行仍可解析完整事件", async () => {
  const seen = [];
  const text =
    ': keepalive\r\nevent: text\r\ndata: {"type":"text",\r\ndata: "delta":"中文"}\r\n\r\nevent: done\ndata: {"type":"done"}\n\n';
  const bytes = new TextEncoder().encode(text);
  const stream = new ReadableStream({
    start(c) {
      for (const byte of bytes) c.enqueue(Uint8Array.of(byte));
      c.close();
    },
  });
  await readEvents(stream, (e) => seen.push(e));
  assert.deepEqual(seen, [{ type: "text", delta: "中文" }, { type: "done" }]);
});
test("未收到终态的EOF必须报告未确认完成", async () => {
  const stream = new ReadableStream({
    start(c) {
      c.enqueue(
        new TextEncoder().encode('data: {"type":"text","delta":"草稿"}\n\n'),
      );
      c.close();
    },
  });
  await assert.rejects(() => readEvents(stream, () => {}), /未确认完成/);
});
test("半帧不当作完整事件且空注释不产生业务数据", () => {
  const seen = [];
  const p = new SSEParser((e) => seen.push(e));
  p.push(': hi\n\ndata: {"type":"te');
  assert.equal(seen.length, 0);
  p.push('xt"}\n\n');
  assert.equal(seen.length, 1);
});
