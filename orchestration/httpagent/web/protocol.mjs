// 页面协议状态独立于DOM，防止ready或其他运行的帧被误认为当前运行成功。
export class WSProtocol {
  constructor() {
    this.runID = null;
    this.terminal = false;
  }
  accept(frame) {
    if (frame.type === "accepted") {
      if (this.runID || typeof frame.run_id !== "string" || !frame.run_id)
        throw new Error("运行确认消息无效");
      this.runID = frame.run_id;
      return { kind: "accepted" };
    }
    if (frame.type === "event") {
      if (
        !this.runID ||
        frame.run_id !== this.runID ||
        frame.event?.run_id !== this.runID
      )
        throw new Error("收到其他运行或缺少身份的事件");
      if (this.terminal) throw new Error("业务终态后收到额外事件");
      this.terminal =
        frame.event.type === "done" || frame.event.type === "error";
      return { kind: "event", event: frame.event };
    }
    if (frame.type === "ready") {
      if (!this.terminal)
        throw new Error("未确认完成：ready之前没有收到业务终态");
      return { kind: "ready" };
    }
    if (frame.type === "error")
      throw new Error(frame.error?.message || "WebSocket协议错误");
    throw new Error("未知WebSocket消息");
  }
}
