import { WSProtocol } from "./protocol.mjs";
import { readEvents } from "./sse.mjs";
const $ = (id) => document.getElementById(id);
let controller = null,
  socket = null,
  runID = null,
  active = false;
function status(text) {
  $("status").textContent = text;
}
function controls(running) {
  active = running;
  $("send").disabled = running;
  $("stop").disabled = !running;
  $("transport").disabled = running;
}
function log(text) {
  const item = document.createElement("li");
  item.textContent = text;
  $("events").append(item);
}
function event(e) {
  if (active && e.type !== "done" && e.type !== "error") status("执行中");
  if (e.run_id) {
    runID = e.run_id;
    $("run-id").textContent = `Run ${runID}`;
  }
  if (e.type === "text") $("answer").textContent += e.delta ?? "";
  if (e.type === "step") log(`${e.tool ?? "Agent"} · ${e.step ?? ""}`);
  if (e.type === "done") {
    const result = e.output?.result;
    $("answer").textContent =
      typeof result === "string" ? result : JSON.stringify(result, null, 2);
    status("已完成");
    log("运行完成");
  }
  if (e.type === "error") {
    status(e.error?.code === "cancelled" ? "已取消" : "执行失败");
    log(e.error?.message ?? "执行失败");
  }
}
async function sse(input) {
  controller = new AbortController();
  const response = await fetch("/api/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ input }),
    signal: controller.signal,
  });
  if (!response.ok) {
    const body = await response.json();
    throw new Error(body.error?.message ?? `HTTP ${response.status}`);
  }
  if (!response.headers.get("content-type")?.startsWith("text/event-stream"))
    throw new Error("响应不是 SSE 事件流");
  await readEvents(response.body, event);
}
async function ws(input) {
  return new Promise((resolve, reject) => {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(`${scheme}//${location.host}/api/ws`, "goagent.v1");
    const protocol = new WSProtocol();
    let terminal = false,
      ready = false;
    socket.onopen = () =>
      socket.send(
        JSON.stringify({ type: "run", request_id: crypto.randomUUID(), input }),
      );
    socket.onmessage = (message) => {
      try {
        const frame = JSON.parse(message.data);
        protocol.accept(frame);
        if (frame.type === "accepted") {
          runID = frame.run_id;
          $("run-id").textContent = `Run ${runID}`;
          status("执行中");
        }
        if (frame.type === "event") {
          event(frame.event);
          terminal ||=
            frame.event.type === "done" || frame.event.type === "error";
        }
        if (frame.type === "error") throw new Error(frame.error.message);
        if (frame.type === "ready") {
          ready = true;
          resolve();
          socket.close();
        }
      } catch (error) {
        reject(error);
        socket.close();
      }
    };
    socket.onerror = () => reject(new Error("WebSocket 连接失败"));
    socket.onclose = () => {
      if (!ready)
        reject(
          new Error(
            terminal
              ? "连接已关闭，清理状态未确认"
              : "连接中断，未确认完成；请勿自动重提",
          ),
        );
    };
  });
}
$("form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const input = $("input").value.trim();
  if (!input || active) return;
  runID = null;
  $("answer").textContent = "";
  $("events").replaceChildren();
  $("run-id").textContent = "等待运行 ID";
  controls(true);
  status("连接中");
  try {
    await ($("transport").value === "ws" ? ws(input) : sse(input));
  } catch (error) {
    if (error.name === "AbortError") {
      status("停止已请求");
      log("已中断客户端连接；服务端实际退出状态未确认");
    } else {
      status("未确认 / 错误");
      log(error.message);
    }
  } finally {
    controller = null;
    socket = null;
    controls(false);
  }
});
$("stop").addEventListener("click", () => {
  status("正在取消");
  $("stop").disabled = true;
  if (socket?.readyState === WebSocket.OPEN && runID)
    socket.send(JSON.stringify({ type: "cancel", run_id: runID }));
  else if (socket) socket.close();
  else controller?.abort();
});
