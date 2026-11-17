// 两个页面共用的辅助函数：接口调用、DOM 构造、轮询。所有服务端数据只经 textContent 渲染。
const POLL_MS = 2000;

// 接口出错时抛出带服务端中文消息的 Error；网络错误保持 TypeError，调用方据此决定是否重试。
async function api(method, path, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(path, init);
  const raw = await resp.text();
  let data = null;
  try {
    data = raw ? JSON.parse(raw) : null;
  } catch (_) {
    // 非 JSON 响应按状态码报错
  }
  if (!resp.ok) {
    throw new Error(data && data.error ? data.error.message : "请求失败：HTTP " + resp.status);
  }
  return data;
}

function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "class") node.className = v;
    else node[k] = v;
  }
  for (const c of children) if (c) node.append(c);
  return node;
}

// 满足服务端的 ID 规则 ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$。
function newId() {
  return crypto.randomUUID().replaceAll("-", "");
}

function showError(node, err) {
  node.textContent = err ? err.message : "";
  node.hidden = !err;
}

// 立即执行一次，之后每 2 秒一次；上一次未结束时跳过，避免请求堆积。
function poll(fn) {
  let busy = false;
  const tick = async () => {
    if (busy) return;
    busy = true;
    try {
      await fn();
    } finally {
      busy = false;
    }
  };
  tick();
  return setInterval(tick, POLL_MS);
}

function fmtTime(s) {
  return s ? s.replace("T", " ").replace(/\.\d+Z$/, "Z") : "";
}
