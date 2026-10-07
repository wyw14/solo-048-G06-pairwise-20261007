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
  const published = data.plans.filter((plan) => plan.status === "published").length;
  return [["运行区域", active], ["在线换热站", online], ["活动事故", incidents], ["已发布计划", published]]
    .map(([name, value]) => `<div class="metric"><span>${name}</span><strong>${value}</strong></div>`).join("");
}

function badge(status) {
  return `<span class="badge badge-${status}">${text(status)}</span>`;
}

const RESULT_LABELS = {
  acknowledged: "已回执", pending: "等待回执", rejected: "已拒绝",
  offline: "离线", stale_epoch: "代际过期", no_receipt: "回执超时",
};

function changeFlags(change) {
  const flags = [];
  if (change.maintenance) flags.push("检修");
  if (change.hold) flags.push("人工保持");
  if (!change.online) flags.push("离线");
  return flags.map((flag) => `<span class="flag">${flag}</span>`).join("");
}

function renderDraft(draft) {
  const approvals = (zones) => zones.map((zone) => {
    const approval = draft.approvals && draft.approvals[zone];
    const state = approval && approval.approved ? "已审批" : "待审批";
    const by = approval && approval.approved_by ? ` (${approval.approved_by})` : "";
    return `<span class="approval">${zone}: <strong>${state}${by}</strong></span>`;
  }).join("");
  const zones = draft.changes
    .map((change) => change.zone_id)
    .filter((zone, index, all) => all.indexOf(zone) === index).sort();
  const rows = draft.changes.map((change) => `<tr>
    <td>${change.name || change.station_id}</td><td>${change.zone_id}</td>
    <td>${change.current_celsius.toFixed(1)} → ${change.proposed_celsius.toFixed(1)} °C</td>
    <td>${change.delta_celsius > 0 ? "+" : ""}${change.delta_celsius.toFixed(1)}</td>
    <td>${change.current_epoch}</td><td>${changeFlags(change)}${change.note ? `<span class="flag">${change.note}</span>` : ""}</td>
  </tr>`).join("");
  return `<article class="panel wide"><h3>目标草案 ${draft.id.slice(0, 8)} ${badge(draft.status)}</h3>
    <dl><dt>来源 revision</dt><dd>${draft.source_revision}</dd><dt>分区审批</dt><dd>${approvals(zones)}</dd></dl>
    <table class="targets"><thead><tr><th>站点</th><th>分区</th><th>当前 → 目标</th><th>偏差</th><th>当前代际</th><th>标记</th></tr></thead><tbody>${rows}</tbody></table>
    ${draft.message ? `<p class="flag">${draft.message}</p>` : ""}</article>`;
}

function renderRelease(release) {
  const success = Object.values(release.results || {}).filter((result) => result.status === "acknowledged").length;
  const pending = Object.values(release.results || {}).filter((result) => result.status === "pending").length;
  const failed = Object.values(release.results || {}).length - success - pending;
  const alert = release.status === "partial_fail" || release.status === "failed" ? " release-alert" : "";
  const rows = Object.values(release.results || {}).sort((a, b) => a.station_id.localeCompare(b.station_id)).map((result) => `<tr>
    <td>${result.station_id}</td><td>${result.zone_id}</td>
    <td>${result.requested_epoch}</td><td>${result.applied_epoch || "-"}</td>
    <td>${result.command_id ? result.command_id.slice(0, 8) : "-"}</td>
    <td><span class="badge badge-${result.status}">${RESULT_LABELS[result.status] || result.status}</span></td>
    <td>${result.detail || ""}</td>
  </tr>`).join("");
  const headline = release.status === "partial_fail"
    ? `<strong>部分失败：${failed} 台控制器未成功，整批不得视为发布成功</strong>`
    : `成功 ${success} / 待回执 ${pending} / 失败 ${failed}`;
  return `<article class="panel wide${alert}"><h3>发布批次 ${release.id.slice(0, 8)} ${badge(release.status)}</h3>
    <dl><dt>草案</dt><dd>${release.draft_id.slice(0, 8)}</dd><dt>回执进度</dt><dd>${headline}</dd></dl>
    <table class="targets"><thead><tr><th>站点</th><th>分区</th><th>下发代际</th><th>回执代际</th><th>命令</th><th>状态</th><th>说明</th></tr></thead><tbody>${rows}</tbody></table>
    ${release.message ? `<p>${release.message}</p>` : ""}</article>`;
}

function pageContent(data) {
  if (page === "network") return data.zones.map((zone) => panel(zone.name, [["阶段", zone.phase], ["泵级", zone.pump_stage], ["旁通", zone.bypass_open ? "开启" : "关闭"], ["压差", (zone.supply_pressure - zone.return_pressure).toFixed(2) + " MPa"]])).join("");
  if (page === "stations") return data.stations.map((station) => panel(station.name, [["区域", station.zone_id], ["通信", station.online ? "在线" : "离线"], ["目标", station.target_celsius + " °C"], ["控制代际", station.controller_epoch], ["人工保持", station.hold ? "是" : "否"]])).join("");
  if (page === "incidents") return data.incidents.length ? data.incidents.map((incident) => panel(incident.kind, [["站点", incident.station_id], ["等级", incident.severity], ["状态", incident.active ? "处理中" : "已解除"], ["说明", incident.message]])).join("") : panel("当前无活动事故", [["隔离会话", data.isolations.length], ["系统状态", "稳定"]]);
  if (page === "dispatch") {
    const plans = data.plans.length ? data.plans.map((plan) => panel(plan.id.slice(0, 8), [["状态", plan.status], ["来源 revision", plan.source_revision], ["站点数", Object.keys(plan.targets || {}).length], ["说明", plan.message || "-"]])).join("") : panel("等待调度计划", [["当前 revision", Math.max(...data.stations.map((station) => station.target_revision))], ["人工保持", data.stations.filter((station) => station.hold).length]]);
    const drafts = (data.release_drafts || []).map(renderDraft).join("");
    const releases = (data.releases || []).map(renderRelease).join("");
    return plans + drafts + releases;
  }
  return "";
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
