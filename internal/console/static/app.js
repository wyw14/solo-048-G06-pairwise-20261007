const page = document.body.dataset.page;
const state = { overview: null };

function text(value) {
  return value === undefined || value === null ? "-" : String(value);
}

function panel(title, rows) {
  const body = rows.map(([name, value]) => `<dt>${name}</dt><dd>${text(value)}</dd>`).join("");
  return `<article class="panel"><h3>${title}</h3><dl>${body}</dl></article>`;
}

function metrics(data) {
  const active = data.zones.filter((zone) => zone.phase === "active").length;
  const online = data.stations.filter((station) => station.online).length;
  const incidents = data.incidents.filter((incident) => incident.active).length;
  const partial = (data.releases || []).filter((item) => item.status === "partial_failure" || item.status === "failed").length;
  const published = (data.releases || []).filter((item) => item.status === "succeeded").length
    + data.plans.filter((plan) => plan.status === "published").length;
  return [["运行区域", active], ["在线换热站", online], ["活动事故", incidents], ["发布异常批次", partial || "0"]]
    .map(([name, value]) => `<div class="metric"><span>${name}</span><strong>${value}</strong></div>`).join("");
}

const releaseStatusText = {
  pending_approval: ["待分区审批", "muted"],
  rejected: ["已驳回", "bad"],
  expired: ["草案过期", "bad"],
  succeeded: ["全部回执", "good"],
  partial_failure: ["部分失败", "bad"],
  failed: ["全部失败", "bad"],
};

function badge(status) {
  const [label, tone] = releaseStatusText[status] || [status, "muted"];
  return `<span class="badge badge-${tone}">${label}</span>`;
}

function releasePanel(item) {
  const rows = [
    ["状态", badge(item.status)],
    ["草案 revision", item.source_revision],
    ["分区", item.zones.map((zone) => `${zone.zone_name}:${zone.decision}`).join("，")],
    ["汇总", item.message || "-"],
  ];
  let body = rows.map(([name, value]) => `<dt>${name}</dt><dd>${value}</dd>`).join("");
  if ((item.results || []).length) {
    const detail = item.results.map((result) => {
      const state = result.acknowledged
        ? `<span class="ok">回执 ${result.receipt_status}</span>`
        : `<span class="fail">失败：${result.error || "无回执"}</span>`;
      return `<li>${result.station_name || result.station_id} · epoch ${result.epoch} · seq ${result.sequence || "-"} · ${result.celsius} °C · ${state}</li>`;
    }).join("");
    body += `<dt>控制器</dt><dd><ul class="receipts">${detail}</ul></dd>`;
  }
  return `<article class="panel ${item.status === "partial_failure" || item.status === "failed" ? "panel-bad" : ""}"><h3>批次 ${item.id.slice(0, 8)}</h3><dl>${body}</dl></article>`;
}

function draftsPanel(data) {
  const drafts = data.target_drafts || [];
  if (!drafts.length) return "";
  const cards = drafts.slice(0, 5).map((draft) => {
    const lines = draft.entries.map((entry) => {
      const flag = entry.changed ? "调整" : "不变";
      const issue = entry.issue ? ` · <span class="fail">${entry.issue}</span>` : "";
      return `<li>${entry.station_name || entry.station_id}：${entry.current_celsius} → ${entry.desired_celsius} °C（${flag}）· epoch ${entry.controller_epoch}${issue}</li>`;
    }).join("");
    return `<article class="panel"><h3>草案 ${draft.id.slice(0, 8)} <span class="muted">rev ${draft.source_revision}</span></h3><ul class="receipts">${lines}</ul></article>`;
  }).join("");
  return `<h2>目标草案</h2><div class="grid">${cards}</div>`;
}

function pageContent(data) {
  if (page === "network") return data.zones.map((zone) => panel(zone.name, [["阶段", zone.phase], ["泵级", zone.pump_stage], ["旁通", zone.bypass_open ? "开启" : "关闭"], ["压差", (zone.supply_pressure - zone.return_pressure).toFixed(2) + " MPa"]])).join("");
  if (page === "stations") return data.stations.map((station) => panel(station.name, [["区域", station.zone_id], ["通信", station.online ? "在线" : "离线"], ["目标", station.target_celsius + " °C"], ["控制代际", station.controller_epoch], ["人工保持", station.hold ? "是" : "否"]])).join("");
  if (page === "incidents") return data.incidents.length ? data.incidents.map((incident) => panel(incident.kind, [["站点", incident.station_id], ["等级", incident.severity], ["状态", incident.active ? "处理中" : "已解除"], ["说明", incident.message]])).join("") : panel("当前无活动事故", [["隔离会话", data.isolations.length], ["系统状态", "稳定"]]);
  const releases = (data.releases || []).slice().sort((a, b) => (b.created_at || "").localeCompare(a.created_at || ""));
  const releaseSection = releases.length
    ? `<h2>站点目标发布</h2><div class="grid">${releases.slice(0, 8).map(releasePanel).join("")}</div>`
    : "";
  const legacy = data.plans.length ? data.plans.map((plan) => panel(plan.id.slice(0, 8), [["状态", plan.status], ["来源 revision", plan.source_revision], ["站点数", Object.keys(plan.targets || {}).length], ["说明", plan.message || "-"]])).join("") : "";
  return draftsPanel(data) + releaseSection + (legacy ? `<h2>调度计划</h2><div class="grid">${legacy}</div>` : "");
}

async function load() {
  const connection = document.querySelector("#connection");
  try {
    const response = await fetch("/api/v1/overview", { cache: "no-store" });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "request failed");
    state.overview = payload.data;
    document.querySelector("#summary").innerHTML = metrics(payload.data);
    document.querySelector("#content").innerHTML = pageContent(payload.data);
    connection.textContent = "实时连接";
  } catch (error) {
    connection.textContent = "连接异常";
    document.querySelector("#content").innerHTML = panel("无法加载状态", [["错误", error.message]]);
  }
}

document.querySelector("#refresh").addEventListener("click", load);
load();
