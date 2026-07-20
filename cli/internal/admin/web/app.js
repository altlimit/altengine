"use strict";

// Minimal dependency-free console for the altengine emulator. It talks to the
// /admin REST API (same origin) and, for the channels playground, the /v1 data plane
// using a dev bearer token (the emulator is dev-open).

const $ = (sel, root = document) => root.querySelector(sel);
const main = $("#main");
const DEV_BEARER = "console-dev-key";

// ---- helpers ----
function h(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "html") e.innerHTML = v;
    else if (k.startsWith("on") && typeof v === "function") e.addEventListener(k.slice(2), v);
    else if (v !== null && v !== undefined && v !== false) e.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    e.append(kid.nodeType ? kid : document.createTextNode(kid));
  }
  return e;
}
function toast(msg, isErr) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast show" + (isErr ? " err" : "");
  setTimeout(() => (t.className = "toast"), 2600);
}
async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const msg = data && data.error ? `${data.error.code}: ${data.error.message}` : `HTTP ${res.status}`;
    const e = new Error(msg); e.data = data; e.status = res.status; throw e;
  }
  return data;
}
const jGet = (p) => api("GET", p);
const jPost = (p, b) => api("POST", p, b);
const jPut = (p, b) => api("PUT", p, b);
const jDel = (p) => api("DELETE", p);
function fmtTime(ms) { return ms ? new Date(ms).toLocaleString() : "—"; }
function parseJSONField(text, fallback) {
  const t = (text || "").trim();
  if (!t) return fallback;
  return JSON.parse(t);
}

// ---- router ----
const views = {};
let current = "overview";
function setView(name) {
  current = name;
  for (const b of document.querySelectorAll("#nav .tab")) b.classList.toggle("active", b.dataset.view === name);
  main.innerHTML = "";
  main.focus();
  (views[name] || views.overview)();
}
$("#nav").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-view]");
  if (b) setView(b.dataset.view);
});

// A reusable "instance picker" for a service.
async function instancePicker(service, onPick, current) {
  const { instances } = await jGet(`/admin/${service}`);
  const sel = h("select", { onchange: () => onPick(sel.value) },
    h("option", { value: "" }, "— select instance —"),
    ...instances.map((i) => h("option", { value: i.id, ...(i.id === current ? { selected: "" } : {}) }, i.name))
  );
  const nameIn = h("input", { placeholder: "new instance name", class: "flex1" });
  const create = h("button", { class: "btn ghost", onclick: async () => {
    if (!nameIn.value.trim()) return;
    try { const i = await jPost(`/admin/${service}`, { name: nameIn.value.trim() }); toast("created " + i.name); onPick(i.id, true); }
    catch (e) { toast(e.message, true); }
  } }, "+ Create");
  return h("div", { class: "row" },
    h("div", { class: "flex1" }, h("label", {}, "Instance"), sel),
    h("div", { class: "flex1" }, h("label", {}, " "), h("div", { class: "row" }, nameIn, create))
  );
}

// ================= OVERVIEW =================
views.overview = async () => {
  main.append(h("h1", {}, "Local emulator"), h("p", { class: "muted" },
    "Four services running locally. Point your SDK at this host and use any Bearer token."));
  const grid = h("div", { class: "grid" });
  main.append(grid);
  for (const svc of ["datastore", "search", "channel", "auth"]) {
    try {
      const { instances } = await jGet(`/admin/${svc}`);
      const label = svc === "channel" ? "channels" : svc;
      const clickable = true;
      grid.append(h("div", { class: "card click", onclick: () => setView(label === "channel" ? "channels" : label) },
        h("h2", {}, label[0].toUpperCase() + label.slice(1)),
        h("div", { class: "mono" }, `${instances.length} instance${instances.length === 1 ? "" : "s"}`),
        h("div", { class: "muted", style: "margin-top:6px;font-size:12px" }, `/v1/${svc}/{instance}`)
      ));
    } catch (e) { /* ignore */ }
  }
  main.append(h("div", { class: "card" },
    h("h2", {}, "Base URLs"),
    h("div", { class: "kv mono" },
      h("span", { class: "muted" }, "Datastore"), h("span", {}, location.origin + "/v1/datastore/{instance}"),
      h("span", { class: "muted" }, "Search"), h("span", {}, location.origin + "/v1/search/{instance}"),
      h("span", { class: "muted" }, "Channels"), h("span", {}, location.origin + "/v1/channel/{instance}"),
      h("span", { class: "muted" }, "Auth"), h("span", {}, location.origin + "/v1/auth/{instance}"),
      h("span", { class: "muted" }, "API keys"), h("span", {}, "Authorization: Bearer <any-token>"),
    )
  ));
};

// ================= DATASTORE =================
const dsState = { instId: null, ns: "default", coll: "" };
views.datastore = async () => {
  main.append(h("h1", {}, "Datastore"));
  main.append(await instancePicker("datastore", (id) => { dsState.instId = id; renderDS(); }, dsState.instId));
  const holder = h("div", { id: "ds-holder" });
  main.append(holder);
  if (dsState.instId) renderDS();
};
async function renderDS() {
  const holder = $("#ds-holder"); if (!holder) return; holder.innerHTML = "";
  if (!dsState.instId) return;
  // namespace + collection pickers
  let namespaces = [];
  try { namespaces = (await jGet(`/admin/datastore/${dsState.instId}/namespaces`)).namespaces.map((n) => n.namespace); } catch {}
  if (!namespaces.includes(dsState.ns)) namespaces.unshift(dsState.ns);
  const nsSel = h("input", { value: dsState.ns, class: "flex1", onchange: (e) => { dsState.ns = e.target.value || "default"; loadColls(); } });
  const collSel = h("select", { onchange: (e) => { dsState.coll = e.target.value; } });
  const collInput = h("input", { placeholder: "collection", class: "flex1", value: dsState.coll, onchange: (e) => { dsState.coll = e.target.value; collSel.value = ""; } });
  async function loadColls() {
    collSel.innerHTML = "";
    collSel.append(h("option", { value: "" }, "— pick collection —"));
    try {
      const { collections } = await jGet(`/admin/datastore/${dsState.instId}/namespaces/${encodeURIComponent(dsState.ns)}/collections`);
      for (const c of collections) collSel.append(h("option", { value: c }, c));
    } catch {}
  }
  collSel.addEventListener("change", () => { if (collSel.value) { dsState.coll = collSel.value; collInput.value = collSel.value; } });
  loadColls();
  holder.append(h("div", { class: "row" },
    h("div", { class: "flex1" }, h("label", {}, "Namespace"), nsSel),
    h("div", { class: "flex1" }, h("label", {}, "Collection"), h("div", { class: "row" }, collSel, collInput))
  ));

  const sub = subtabs(["Query", "Aggregate", "Insert", "Indexes"], (name) => renderDSTab(name, holder));
  holder.append(sub.bar, sub.body);
  renderDSTab("Query", holder);
}
function renderDSTab(name, holder) {
  const body = holder.querySelector(".subtab-body"); body.innerHTML = "";
  const base = () => `/admin/datastore/${dsState.instId}/namespaces/${encodeURIComponent(dsState.ns)}/collections/${encodeURIComponent(dsState.coll)}`;
  const needColl = () => { if (!dsState.coll) { toast("pick a collection first", true); return false; } return true; };

  if (name === "Query") {
    const where = h("textarea", { placeholder: '[{"field":"role","op":"=","value":"admin"}]' });
    const order = h("input", { placeholder: 'order e.g. __updated__:desc' });
    const out = h("div");
    const run = h("button", { class: "btn", onclick: async () => {
      if (!needColl()) return;
      try {
        const req = { where: parseJSONField(where.value, []), limit: 25 };
        if (order.value.trim()) { const [f, d] = order.value.split(":"); req.order = [{ field: f, dir: d || "asc" }]; }
        const res = await jPost(base() + "/query", req);
        renderDocs(out, res.documents || [], base(), () => renderDSTab("Query", holder), res);
      } catch (e) { out.innerHTML = ""; out.append(errBox(e)); }
    } }, "Run query");
    body.append(h("label", {}, "where (JSON array)"), where, h("label", {}, "order"), order, h("div", { class: "row", style: "margin-top:8px" }, run), out);
  }

  if (name === "Aggregate") {
    const group = h("input", { placeholder: "group fields, comma sep e.g. role" });
    const metrics = h("textarea", { placeholder: '[{"fn":"count","as":"n"},{"fn":"avg","field":"age","as":"avg_age"}]' });
    const where = h("textarea", { placeholder: "where (optional) []" });
    const out = h("div");
    const run = h("button", { class: "btn", onclick: async () => {
      if (!needColl()) return;
      try {
        const req = { metrics: parseJSONField(metrics.value, [{ fn: "count", as: "n" }]) };
        if (group.value.trim()) req.group = group.value.split(",").map((s) => s.trim()).filter(Boolean);
        req.where = parseJSONField(where.value, []);
        const res = await jPost(base() + "/aggregate", req);
        out.innerHTML = ""; out.append(h("pre", {}, JSON.stringify(res.groups, null, 2)));
      } catch (e) { out.innerHTML = ""; out.append(errBox(e)); }
    } }, "Run aggregate");
    body.append(h("label", {}, "group"), group, h("label", {}, "metrics (JSON)"), metrics, h("label", {}, "where (JSON)"), where, h("div", { class: "row", style: "margin-top:8px" }, run), out);
  }

  if (name === "Insert") {
    const key = h("input", { placeholder: "key (optional — auto-generated)" });
    const data = h("textarea", { placeholder: '{"role":"admin","age":40}' });
    const run = h("button", { class: "btn", onclick: async () => {
      if (!needColl()) return;
      try {
        const doc = { data: parseJSONField(data.value, {}) };
        if (key.value.trim()) doc.key = key.value.trim();
        const res = await jPost(base() + "/documents", { documents: [doc] });
        toast("saved key " + res.keys[0]); data.value = ""; key.value = "";
      } catch (e) { toast(e.message, true); }
    } }, "Insert / upsert");
    body.append(h("label", {}, "key"), key, h("label", {}, "data (JSON object)"), data, h("div", { class: "row", style: "margin-top:8px" }, run));
  }

  if (name === "Indexes") {
    const list = h("div");
    const fields = h("input", { placeholder: "fields, comma sep e.g. status, __updated__" });
    const uniq = h("input", { type: "checkbox" });
    const add = h("button", { class: "btn ghost", onclick: async () => {
      if (!needColl()) return;
      try {
        await jPost(base() + "/indexes", { fields: fields.value.split(",").map((s) => s.trim()).filter(Boolean), unique: uniq.checked });
        toast("index created"); fields.value = ""; loadIx();
      } catch (e) { toast(e.message, true); }
    } }, "+ Add index");
    async function loadIx() {
      list.innerHTML = "";
      if (!dsState.coll) { list.append(h("div", { class: "empty" }, "pick a collection")); return; }
      const { indexes } = await jGet(base() + "/indexes");
      if (!indexes.length) { list.append(h("div", { class: "empty" }, "no indexes")); return; }
      const t = h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "id"), h("th", {}, "fields"), h("th", {}, "unique"), h("th", {}, ""))));
      const tb = h("tbody");
      for (const ix of indexes) {
        tb.append(h("tr", {},
          h("td", { class: "mono" }, String(ix.id)),
          h("td", { class: "mono" }, ix.fields.join(", ")),
          h("td", {}, ix.unique ? "yes" : "—"),
          h("td", {}, h("button", { class: "btn danger sm", onclick: async () => { await jDel(base() + "/indexes/" + ix.id); loadIx(); } }, "Drop"))
        ));
      }
      t.append(tb); list.append(h("div", { class: "tablewrap" }, t));
    }
    loadIx();
    body.append(h("div", { class: "row" }, h("div", { class: "flex1" }, h("label", {}, "fields"), fields), h("label", { class: "row", style: "align-self:end;gap:6px" }, uniq, "unique"), add), list);
  }
}
function renderDocs(out, docs, base, reload, meta) {
  out.innerHTML = "";
  if (meta && meta.auto_indexed) out.append(h("div", { class: "pill ok" }, "auto-indexed: " + meta.auto_indexed.fields.join(", ")));
  if (!docs.length) { out.append(h("div", { class: "empty" }, "no documents")); return; }
  const t = h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "key"), h("th", {}, "data"), h("th", {}, ""))));
  const tb = h("tbody");
  for (const d of docs) {
    tb.append(h("tr", {},
      h("td", { class: "mono" }, d.key),
      h("td", {}, h("pre", { style: "margin:0;max-height:160px" }, JSON.stringify(d.data, null, 2))),
      h("td", {}, h("button", { class: "btn danger sm", onclick: async () => {
        try { await jPost(base + "/documents/delete", { keys: [d.key] }); toast("deleted"); reload(); } catch (e) { toast(e.message, true); }
      } }, "Delete"))
    ));
  }
  t.append(tb); out.append(h("div", { class: "tablewrap" }, t));
}

// ================= SEARCH =================
const srState = { instId: null, index: "" };
views.search = async () => {
  main.append(h("h1", {}, "Search"));
  main.append(await instancePicker("search", (id) => { srState.instId = id; renderSearch(); }, srState.instId));
  main.append(h("div", { id: "sr-holder" }));
  if (srState.instId) renderSearch();
};
async function renderSearch() {
  const holder = $("#sr-holder"); if (!holder) return; holder.innerHTML = "";
  const indexSel = h("select", {});
  const indexInput = h("input", { placeholder: "index name", class: "flex1", value: srState.index, onchange: (e) => { srState.index = e.target.value; } });
  indexSel.addEventListener("change", () => { if (indexSel.value) { srState.index = indexSel.value; indexInput.value = indexSel.value; } });
  try {
    const { indexes } = await jGet(`/admin/search/${srState.instId}/indexes`);
    indexSel.append(h("option", { value: "" }, "— pick index —"));
    for (const i of indexes) indexSel.append(h("option", { value: i.name }, i.name));
  } catch {}
  holder.append(h("div", { class: "row" },
    h("div", { class: "flex1" }, h("label", {}, "Index"), h("div", { class: "row" }, indexSel, indexInput))));

  const sub = subtabs(["Search", "Insert"], (name) => renderSearchTab(name, holder));
  holder.append(sub.bar, sub.body);
  renderSearchTab("Search", holder);
}
function renderSearchTab(name, holder) {
  const body = holder.querySelector(".subtab-body"); body.innerHTML = "";
  const base = () => `/admin/search/${srState.instId}/indexes/${encodeURIComponent(srState.index)}`;
  const needIx = () => { if (!srState.index) { toast("pick an index", true); return false; } return true; };

  if (name === "Search") {
    const q = h("input", { placeholder: 'query e.g.  genre:comedy rating > 3   (empty = all)' });
    const facets = h("input", { placeholder: "facet_discover count (optional)" });
    const out = h("div");
    const run = h("button", { class: "btn", onclick: async () => {
      if (!needIx()) return;
      try {
        const req = { query: q.value, limit: 25 };
        if (facets.value.trim()) req.facet_discover = parseInt(facets.value);
        const res = await jPost(base() + "/search", req);
        out.innerHTML = "";
        out.append(h("div", { class: "muted", style: "margin:6px 0" }, `${res.total_hits}${res.total_hits_exact ? "" : "+"} hits`));
        if (res.facets && res.facets.length) out.append(h("pre", {}, "facets: " + JSON.stringify(res.facets, null, 2)));
        const t = h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "id"), h("th", {}, "rank"), h("th", {}, "document"), h("th", {}, ""))));
        const tb = h("tbody");
        for (const r of res.results) {
          tb.append(h("tr", {},
            h("td", { class: "mono" }, r.id),
            h("td", { class: "mono" }, String(r.rank)),
            h("td", {}, h("pre", { style: "margin:0;max-height:160px" }, JSON.stringify(r.document, null, 2))),
            h("td", {}, h("button", { class: "btn danger sm", onclick: async () => { await jPost(base() + "/documents/delete", { ids: [r.id] }); toast("deleted"); run.click(); } }, "Delete"))
          ));
        }
        t.append(tb);
        out.append(res.results.length ? h("div", { class: "tablewrap" }, t) : h("div", { class: "empty" }, "no results"));
      } catch (e) { out.innerHTML = ""; out.append(errBox(e)); }
    } }, "Search");
    body.append(h("label", {}, "query"), q, h("label", {}, "facet_discover"), facets, h("div", { class: "row", style: "margin-top:8px" }, run), out);
  }

  if (name === "Insert") {
    const doc = h("textarea", { style: "min-height:200px", placeholder: '{\n  "id": "m1",\n  "fields": [\n    {"name":"title","type":"text","value":"Up in the Air"},\n    {"name":"genre","type":"atom","value":"drama"},\n    {"name":"rating","type":"number","value":4}\n  ],\n  "facets": [{"name":"genre","type":"atom","value":"drama"}]\n}' });
    const run = h("button", { class: "btn", onclick: async () => {
      if (!needIx()) return;
      try { const res = await jPost(base() + "/documents", { documents: [parseJSONField(doc.value, {})] }); toast("saved " + res.ids[0]); }
      catch (e) { toast(e.message, true); }
    } }, "Put document");
    body.append(h("label", {}, "document (JSON)"), doc, h("div", { class: "row", style: "margin-top:8px" }, run));
  }
}

// ================= CHANNELS =================
const chState = { instId: null, name: null, ws: null };
views.channels = async () => {
  main.append(h("h1", {}, "Channels"));
  main.append(await instancePicker("channel", async (id) => {
    chState.instId = id;
    const inst = (await jGet(`/admin/channel`)).instances.find((i) => i.id === id);
    chState.name = inst ? inst.name : null;
    renderChannels();
  }, chState.instId));
  main.append(h("div", { id: "ch-holder" }));
  if (chState.instId) renderChannels();
};
function renderChannels() {
  const holder = $("#ch-holder"); if (!holder) return; holder.innerHTML = "";
  if (!chState.name) return;
  const channels = h("input", { placeholder: "channels, comma sep e.g. room1,room2", value: "room1" });
  const pub = h("input", { type: "checkbox", checked: "" });
  const log = h("div", { class: "card", style: "height:240px;overflow:auto" });
  function line(cls, text) { log.prepend(h("div", { class: "wsline " + cls }, `[${new Date().toLocaleTimeString()}] ${text}`)); }

  const connectBtn = h("button", { class: "btn", onclick: async () => {
    if (chState.ws) { chState.ws.close(); return; }
    const list = channels.value.split(",").map((s) => s.trim()).filter(Boolean);
    let tok;
    try {
      tok = await fetch(`/v1/channel/${encodeURIComponent(chState.name)}/tokens`, {
        method: "POST", headers: { "Content-Type": "application/json", "Authorization": "Bearer " + DEV_BEARER },
        body: JSON.stringify({ channels: list, publish: pub.checked, presence_id: "console" }),
      }).then((r) => r.json());
    } catch (e) { toast("token mint failed", true); return; }
    if (tok.error) { toast(tok.error.message, true); return; }
    const ws = new WebSocket(tok.ws_url);
    chState.ws = ws;
    connectBtn.textContent = "Disconnect";
    ws.onopen = () => line("sys", "connected: " + list.join(", "));
    ws.onmessage = (ev) => line("in", ev.data);
    ws.onclose = () => { line("sys", "disconnected"); chState.ws = null; connectBtn.textContent = "Connect"; };
    ws.onerror = () => line("sys", "error");
  } }, "Connect");

  const pubChan = h("input", { placeholder: "channel", value: "room1", class: "flex1" });
  const pubData = h("input", { placeholder: 'data JSON e.g. {"hello":"world"}', value: '{"hello":"world"}', class: "flex1" });
  const pubHTTP = h("button", { class: "btn ghost", onclick: async () => {
    try {
      const res = await fetch(`/v1/channel/${encodeURIComponent(chState.name)}/publish`, {
        method: "POST", headers: { "Content-Type": "application/json", "Authorization": "Bearer " + DEV_BEARER },
        body: JSON.stringify({ channel: pubChan.value, data: JSON.parse(pubData.value || "null") }),
      }).then((r) => r.json());
      line("out", "HTTP publish -> delivered " + (res.delivered ?? "?"));
    } catch (e) { toast("publish failed: " + e.message, true); }
  } }, "Publish (HTTP)");
  const pubWS = h("button", { class: "btn ghost", onclick: () => {
    if (!chState.ws) { toast("connect first", true); return; }
    chState.ws.send(JSON.stringify({ type: "publish", channel: pubChan.value, data: JSON.parse(pubData.value || "null") }));
    line("out", "WS publish -> " + pubChan.value);
  } }, "Publish (WS)");

  holder.append(
    h("div", { class: "card" },
      h("div", { class: "row" },
        h("div", { class: "flex1" }, h("label", {}, "Channels"), channels),
        h("label", { class: "row", style: "align-self:end;gap:6px" }, pub, "publish-capable"),
        h("div", { style: "align-self:end" }, connectBtn))),
    h("div", { class: "card" },
      h("div", { class: "row" }, pubChan, pubData),
      h("div", { class: "row", style: "margin-top:8px" }, pubHTTP, pubWS)),
    h("h2", {}, "Messages"), log
  );
}

// ================= AUTH =================
// An auth instance auto-creates on first use, but its DEFAULT config collects only an email
// and grants no access — so a client app signs a user up and then gets 403 on every data
// call. This tab is where you fix that: edit the sign-up form and the access rules, and see
// the end users that result.
const authState = { instId: null, name: null };

views.auth = async () => {
  main.append(h("h1", {}, "Auth"),
    h("p", { class: "muted" },
      "End-user accounts and identity tokens. A signed-in user's id_token is what your app sends to " +
      "the datastore and channel planes; the access rules below decide what it may reach."));
  main.append(await instancePicker("auth", async (id) => {
    authState.instId = id;
    const inst = (await jGet(`/admin/auth`)).instances.find((i) => i.id === id);
    authState.name = inst ? inst.name : null;
    renderAuth();
  }, authState.instId));
  main.append(h("div", { id: "auth-holder" }));
  if (authState.instId) renderAuth();
};

async function renderAuth() {
  const holder = $("#auth-holder"); if (!holder) return; holder.innerHTML = "";
  if (!authState.instId) return;

  const inst = (await jGet(`/admin/auth`)).instances.find((i) => i.id === authState.instId);
  if (!inst) return;

  // ---- config editor ----
  const editor = h("textarea", {
    class: "mono", rows: "18", style: "width:100%",
    spellcheck: "false", "aria-label": "Auth instance configuration (JSON)",
  });
  editor.value = JSON.stringify(inst.config || {}, null, 2);

  const save = h("button", { class: "btn", onclick: async () => {
    let cfg;
    try { cfg = JSON.parse(editor.value); }
    catch (e) { toast("invalid JSON: " + e.message, true); return; }
    try {
      await jPut(`/admin/auth/${authState.instId}/config`, { config: cfg });
      toast("config saved");
      renderAuth();
    } catch (e) { toast(e.message, true); }
  } }, "Save config");

  holder.append(h("div", { class: "card" },
    h("h2", {}, "Configuration"),
    h("p", { class: "muted", style: "font-size:12px;margin-top:0" },
      "signup.identityField names the unique login handle; every other collected field becomes a " +
      "claim you can reference from rules as $auth.claims.X. access is keyed \"datastore:<instance>\" " +
      "or \"channel:<instance>\" — an instance with no entry is denied."),
    editor,
    h("div", { class: "row", style: "margin-top:8px" }, save,
      h("span", { class: "muted mono", style: "font-size:12px" }, `/v1/auth/${inst.name}`)),
  ));

  // ---- end users ----
  const usersCard = h("div", { class: "card" }, h("h2", {}, "End users"));
  holder.append(usersCard);
  try {
    const { users } = await jGet(`/admin/auth/${authState.instId}/users`);
    if (!users.length) {
      usersCard.append(h("p", { class: "muted" }, "No end users yet — sign one up from your app."));
    } else {
      const rows = users.map((u) => h("tr", {},
        h("td", { class: "mono" }, u.identifier),
        h("td", { class: "mono", style: "font-size:11px" }, u.uid),
        h("td", { class: "mono", style: "font-size:11px" }, JSON.stringify(u.claims || {})),
        h("td", {}, u.disabled ? "disabled" : "active"),
        h("td", {}, fmtTime(u.created)),
        h("td", {}, h("button", { class: "btn ghost", onclick: async () => {
          if (!confirm(`Delete ${u.identifier}?`)) return;
          try { await jDel(`/admin/auth/${authState.instId}/users/${encodeURIComponent(u.uid)}`); toast("deleted"); renderAuth(); }
          catch (e) { toast(e.message, true); }
        } }, "Delete")),
      ));
      usersCard.append(h("table", { class: "tbl" },
        h("thead", {}, h("tr", {},
          h("th", { scope: "col" }, "Identifier"), h("th", { scope: "col" }, "UID"),
          h("th", { scope: "col" }, "Claims"), h("th", { scope: "col" }, "Status"),
          h("th", { scope: "col" }, "Created"), h("th", { scope: "col" }, ""))),
        h("tbody", {}, ...rows)));
    }
  } catch (e) {
    usersCard.append(h("p", { class: "muted" }, "Could not load users: " + e.message));
  }
}

// ================= API KEYS =================
views.keys = async () => {
  main.append(h("h1", {}, "API keys"));
  main.append(h("p", { class: "muted" }, "The emulator is dev-open: any Bearer token works. Mint scoped keys here to test grant enforcement."));
  const list = h("div");
  const nameIn = h("input", { placeholder: "key name", class: "flex1" });
  const grantSel = h("select", {}, ...["full", "write", "read"].map((l) => h("option", { value: l }, l)));
  const svcAll = h("input", { type: "checkbox", checked: "" });
  const create = h("button", { class: "btn", onclick: async () => {
    const grants = {};
    if (svcAll.checked) { grants.search = grantSel.value; grants.datastore = grantSel.value; grants.channel = grantSel.value; }
    try {
      const k = await jPost("/admin/keys", { name: nameIn.value.trim(), grants });
      showKey(k); nameIn.value = ""; load();
    } catch (e) { toast(e.message, true); }
  } }, "Mint key");
  function showKey(k) {
    main.querySelector("#key-reveal")?.remove();
    main.append(h("div", { class: "card", id: "key-reveal" },
      h("h2", {}, "New key (shown once)"),
      h("pre", {}, k.key),
      h("div", { class: "muted" }, "grants: " + JSON.stringify(k.grants))));
  }
  async function load() {
    list.innerHTML = "";
    const { api_keys } = await jGet("/admin/keys");
    if (!api_keys.length) { list.append(h("div", { class: "empty" }, "no keys minted")); return; }
    const t = h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "name"), h("th", {}, "prefix"), h("th", {}, "grants"), h("th", {}, "created"), h("th", {}, ""))));
    const tb = h("tbody");
    for (const k of api_keys) {
      tb.append(h("tr", {},
        h("td", {}, k.name || "—"),
        h("td", { class: "mono" }, k.prefix),
        h("td", { class: "mono" }, JSON.stringify(k.grants)),
        h("td", { class: "muted" }, fmtTime(k.created_at)),
        h("td", {}, h("button", { class: "btn danger sm", onclick: async () => { await jDel("/admin/keys/" + k.id); load(); } }, "Revoke"))
      ));
    }
    t.append(tb); list.append(h("div", { class: "tablewrap" }, t));
  }
  main.append(h("div", { class: "card" },
    h("div", { class: "row" },
      h("div", { class: "flex1" }, h("label", {}, "Name"), nameIn),
      h("div", {}, h("label", {}, "Level"), grantSel),
      h("label", { class: "row", style: "align-self:end;gap:6px" }, svcAll, "all services"),
      h("div", { style: "align-self:end" }, create))),
    list);
  load();
};

// ---- small widgets ----
function subtabs(names, onPick) {
  const bar = h("div", { class: "subtabs" });
  const body = h("div", { class: "subtab-body" });
  const btns = names.map((n) => h("button", { class: "subtab", onclick: () => { for (const b of btns) b.classList.toggle("active", b === btn0(n)); onPick(n); } }, n));
  function btn0(n) { return btns[names.indexOf(n)]; }
  btns[0].classList.add("active");
  bar.append(...btns);
  return { bar, body };
}
function errBox(e) {
  return h("div", { class: "card", style: "border-color:var(--err)" },
    h("strong", { style: "color:var(--err)" }, "Error"),
    h("pre", { style: "margin:6px 0 0" }, e.message + (e.data && e.data.error && e.data.error.details ? "\n" + JSON.stringify(e.data.error.details, null, 2) : "")));
}

setView("overview");
