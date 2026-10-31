import test from "node:test";
import assert from "node:assert/strict";
import { WSProtocol } from "./protocol.mjs";

test("没有业务终态的ready必须报告未确认而非成功", () => {
  const protocol = new WSProtocol();
  protocol.accept({ type: "accepted", run_id: "r1" });
  assert.throws(() => protocol.accept({ type: "ready" }), /未确认完成/);
});
test("只接受当前运行的唯一终态，随后ready才代表可复用", () => {
  const protocol = new WSProtocol();
  protocol.accept({ type: "accepted", run_id: "r1" });
  protocol.accept({
    type: "event",
    run_id: "r1",
    event: { type: "text", run_id: "r1", delta: "中文" },
  });
  protocol.accept({
    type: "event",
    run_id: "r1",
    event: { type: "done", run_id: "r1", output: { result: "完成" } },
  });
  assert.equal(protocol.accept({ type: "ready" }).kind, "ready");
  assert.throws(
    () =>
      protocol.accept({
        type: "event",
        run_id: "r1",
        event: { type: "done", run_id: "r1" },
      }),
    /终态/,
  );
});
test("其他运行的帧不能污染当前页面", () => {
  const protocol = new WSProtocol();
  protocol.accept({ type: "accepted", run_id: "r1" });
  assert.throws(
    () =>
      protocol.accept({
        type: "event",
        run_id: "r2",
        event: { type: "done", run_id: "r2" },
      }),
    /运行/,
  );
});
