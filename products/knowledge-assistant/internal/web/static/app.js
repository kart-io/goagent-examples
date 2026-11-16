// 页面只调用 /api/* 产品接口与 /v1/* 管理 API；令牌即用户名。
const $ = (id) => document.getElementById(id);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const terminal = new Set(["succeeded", "failed", "canceled", "attention"]);
let sessionID = null;

function status(text) {
  $("status").textContent = text;
}

function user() {
  const name = $("user").value.trim();
  if (!name) throw new Error("请先填写用户名");
  localStorage.setItem("ka-user", name);
  return name;
}

async function api(method, path, body) {
  const headers = { Authorization: `Bearer ${user()}` };
  if (method !== "GET") headers["Idempotency-Key"] = crypto.randomUUID();
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error?.message ?? `HTTP ${res.status}`);
  return data;
}

async function ensureSession() {
  if (sessionID) return sessionID;
  const agent = await api("POST", "/api/agent");
  let se = await api("POST", "/v1/sessions", { agent_id: agent.agent_id });
  while (se.status !== "ready") {
    await sleep(1000);
    se = await api("GET", `/v1/sessions/${se.id}`);
  }
  sessionID = se.id;
  return sessionID;
}

function render(task) {
  $("task-state").textContent = `任务 ${task.id} · ${task.state}`;
  if (task.state === "waiting_approval") {
    $("answer").textContent = "该操作需要审批，请在下方“待审批”中处理。";
    return;
  }
  if (task.state === "succeeded") {
    const r = task.result;
    $("answer").textContent = typeof r === "string" ? r : JSON.stringify(r, null, 2);
  } else if (terminal.has(task.state)) {
    $("answer").textContent = `任务未成功结束：${task.state}${task.error_kind ? `（${task.error_kind}）` : ""}`;
  }
}

async function ask(e) {
  e.preventDefault();
  $("send").disabled = true;
  try {
    status("提交中");
    const session_id = await ensureSession();
    let task = await api("POST", "/v1/tasks", { session_id, input: $("input").value });
    $("answer").textContent = "思考中…";
    status("执行中");
    while (!terminal.has(task.state)) {
      render(task);
      if (task.state === "waiting_approval") await loadApprovals();
      await sleep(1000);
      task = await api("GET", `/v1/tasks/${task.id}`);
    }
    render(task);
    status("已完成");
    await loadMemories();
  } catch (err) {
    status(err.message);
  } finally {
    $("send").disabled = false;
  }
}

function item(text, buttons) {
  const li = document.createElement("li");
  const span = document.createElement("span");
  span.textContent = text;
  li.append(span);
  for (const [label, cls, fn] of buttons) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.className = cls;
    b.onclick = async () => {
      b.disabled = true;
      try {
        await fn();
      } catch (err) {
        status(err.message);
        b.disabled = false;
      }
    };
    li.append(b);
  }
  return li;
}

function fill(id, nodes, empty) {
  const list = $(id);
  list.replaceChildren(...nodes);
  if (!nodes.length) {
    const li = document.createElement("li");
    li.className = "empty";
    li.textContent = empty;
    list.append(li);
  }
}

async function decide(ap, decision) {
  await api("POST", `/v1/approvals/${ap.id}/decision`, {
    expected_revision: ap.revision,
    decision,
    reason: decision === "approve" ? "页面批准" : "页面驳回",
  });
  await loadApprovals();
}

async function loadApprovals() {
  const page = await api("GET", "/v1/approvals?status=pending");
  fill(
    "approvals",
    page.items.map((ap) =>
      item(`${ap.wait?.info?.tool ?? "操作"}：${ap.wait?.info?.reason ?? ""}`, [
        ["批准", "", () => decide(ap, "approve")],
        ["拒绝", "danger", () => decide(ap, "reject")],
      ]),
    ),
    "没有待审批的操作",
  );
}

async function forget(id) {
  await api("DELETE", `/api/memories/${encodeURIComponent(id)}`);
  await loadMemories();
}

async function loadMemories(e) {
  e?.preventDefault();
  const q = $("memory-q").value.trim();
  const data = await api("GET", `/api/memories?q=${encodeURIComponent(q)}`);
  const row = (r) => item(r.content, [["删除", "danger", () => forget(r.id)]]);
  fill("pinned", data.pinned.map(row), "没有置顶记忆");
  fill("matches", data.matches.map(row), q ? "没有匹配的记忆" : "输入关键词后搜索");
}

async function refresh() {
  if (!$("user").value.trim()) return;
  try {
    await Promise.all([loadApprovals(), loadMemories()]);
    status("就绪");
  } catch (err) {
    status(err.message);
  }
}

$("user").value = localStorage.getItem("ka-user") ?? "";
$("user").addEventListener("change", () => {
  sessionID = null;
  refresh();
});
$("ask").addEventListener("submit", ask);
$("new-session").addEventListener("click", () => {
  sessionID = null;
  status("下次提问将开启新会话");
});
$("refresh-approvals").addEventListener("click", () => loadApprovals().catch((err) => status(err.message)));
$("memory-search").addEventListener("submit", (e) => loadMemories(e).catch((err) => status(err.message)));
refresh();
