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
// A view that polls registers its stop function here; leaving the view stops it.
let viewCleanup = null;
function setView(name) {
  if (typeof viewCleanup === "function") { viewCleanup(); viewCleanup = null; }
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
  for (const svc of ["datastore", "search", "channel", "auth", "mailbox"]) {
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
      h("span", { class: "muted" }, "Mailbox"), h("span", {}, location.origin + "/v1/mailbox/{instance}"),
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
      const { collections, cursor } = await jGet(`/admin/datastore/${dsState.instId}/namespaces/${encodeURIComponent(dsState.ns)}/collections`);
      for (const c of collections) collSel.append(h("option", { value: c }, c));
      // A page: say so, and the field beside the list takes any other name.
      if (cursor) collSel.append(h("option", { value: "", disabled: true }, `first ${collections.length} shown — type a name for others`));
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
    const ws = new WebSocket(tok.ws_endpoint, tok.protocols);
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
      const rows = users.map((u) => {
        // Claims are editable because adding a sign-up field does NOT backfill users who
        // registered before it — and a rule that stamps a missing claim is a hard deny, so
        // those accounts are otherwise stuck.
        const claims = h("input", {
          class: "mono flex1", style: "font-size:11px",
          value: JSON.stringify(u.claims || {}),
          "aria-label": `Claims for ${u.identifier}`,
        });
        const saveClaims = h("button", { class: "btn ghost", onclick: async () => {
          let parsed;
          try { parsed = JSON.parse(claims.value); }
          catch (e) { toast("invalid JSON: " + e.message, true); return; }
          if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) { toast("claims must be a JSON object", true); return; }
          try {
            await jPut(`/admin/auth/${authState.instId}/users/${encodeURIComponent(u.uid)}/claims`, { claims: parsed });
            toast("claims saved");
            renderAuth();
          } catch (e) { toast(e.message, true); }
        } }, "Save");
        return h("tr", {},
          h("td", { class: "mono" }, u.identifier),
          h("td", { class: "mono", style: "font-size:11px" }, u.uid),
          h("td", {}, h("div", { class: "row" }, claims, saveClaims)),
          h("td", {}, u.disabled ? "disabled" : "active"),
          h("td", {}, fmtTime(u.created)),
          h("td", {}, h("button", { class: "btn ghost", onclick: async () => {
            if (!confirm(`Delete ${u.identifier}?`)) return;
            try { await jDel(`/admin/auth/${authState.instId}/users/${encodeURIComponent(u.uid)}`); toast("deleted"); renderAuth(); }
            catch (e) { toast(e.message, true); }
          } }, "Delete")),
        );
      });
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

// ================= MAILBOX =================
// Mail never leaves the emulator. Test mail goes in through the same inbound path as /inject
// (limits, accept mode, caps and routes all apply); sends land in Sent, and with loopback on a
// send to a local address arrives in that mailbox.
const mbState = { instId: null, tab: "Inbox", open: null, pendingOpen: null };
let uidSeq = 0;
const nid = (p) => `${p}-${++uidSeq}`;
// A labelled control: the label is tied to the control by id.
function field(labelText, control, cls = "flex1") {
  if (!control.id) control.id = nid("f");
  return h("div", { class: cls }, h("label", { for: control.id }, labelText), control);
}
function checkbox(labelText, checked) {
  const cb = h("input", { type: "checkbox", ...(checked ? { checked: "" } : {}) });
  return { cb, el: h("label", { class: "row", style: "gap:6px;color:var(--text)" }, cb, labelText) };
}
function fmtBytes(n) {
  if (n == null) return "—";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}
function fileToBase64(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result).replace(/^data:[^,]*,/, ""));
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}

views.mailbox = async () => {
  main.append(h("h1", {}, "Mailbox"),
    h("p", { class: "muted" },
      "Email addresses an app or agent owns. Nothing sent from here leaves your machine: deliver test mail below, and read what your app sends under Sent."));
  main.append(await instancePicker("mailbox", (id) => { mbState.instId = id; mbState.open = null; renderMailbox(); }, mbState.instId));
  main.append(h("div", { id: "mb-holder" }));
  if (mbState.instId) renderMailbox();
};

async function renderMailbox() {
  const holder = $("#mb-holder"); if (!holder) return; holder.innerHTML = "";
  if (typeof viewCleanup === "function") { viewCleanup(); viewCleanup = null; }
  if (!mbState.instId) return;
  let info;
  try { info = await jGet(`/admin/mailbox/${mbState.instId}/info`); }
  catch (e) { holder.append(errBox(e)); return; }

  const loop = checkbox("Loop back sends to local addresses", info.loopback);
  loop.cb.addEventListener("change", async () => {
    try { await jPut("/admin/mailbox-settings", { loopback: loop.cb.checked }); toast("loopback " + (loop.cb.checked ? "on" : "off")); }
    catch (e) { toast(e.message, true); }
  });
  holder.append(h("div", { class: "card" },
    h("div", { class: "kv mono" },
      h("span", { class: "muted" }, "Address"), h("span", {}, info.address || "— (instance name is not a valid address slug; use inject with a bare name)"),
      h("span", { class: "muted" }, "Every name"), h("span", {}, info.pattern || "—"),
      h("span", { class: "muted" }, "Messages"), h("span", {}, `${info.message_count} (${fmtBytes(info.stored_bytes)} stored)`),
      h("span", { class: "muted" }, "Modes"), h("span", {}, `accept ${info.config.settings.acceptMode}, send ${info.config.settings.sendMode}, keep ${info.config.settings.retentionDays} days`),
    ),
    h("div", { style: "margin-top:8px" }, loop.el)));

  const names = ["Inbox", "Sent", "Send", "Compose", "Upload .eml", "Settings & routes", "Addresses"];
  const sub = subtabs(names, (name) => { mbState.tab = name; mbState.open = null; renderMailboxTab(name, holder, info); });
  holder.append(sub.bar, sub.body);
  renderMailboxTab("Inbox", holder, info);
}

function renderMailboxTab(name, holder, info) {
  if (typeof viewCleanup === "function") { viewCleanup(); viewCleanup = null; }
  const body = holder.querySelector(".subtab-body"); body.innerHTML = "";
  if (name === "Inbox") return mailList(body, info, "in");
  if (name === "Sent") return mailList(body, info, "out");
  if (name === "Send") return body.append(mailSendForm(null, (sent) => { mbState.pendingOpen = sent.id; openTab("Sent"); }));
  if (name === "Compose") return mailCompose(body, info);
  if (name === "Upload .eml") return mailUpload(body, info);
  if (name === "Settings & routes") return mailSettings(body, info);
  if (name === "Addresses") return mailAddresses(body, info);
}

// The message list, newest first, refreshed every few seconds while it is on screen.
function mailList(body, info, direction) {
  const base = `/admin/mailbox/${mbState.instId}`;
  const listBox = h("div", { "aria-live": "polite" });
  const view = h("div");
  let cursor = null, rows = [], sig = "";
  const more = h("button", { class: "btn ghost", onclick: () => load(true) }, "Load more");

  function draw() {
    listBox.innerHTML = "";
    if (!rows.length) {
      listBox.append(h("div", { class: "empty" }, direction === "in"
        ? "No mail yet. Compose a test email, upload an .eml, or inject one from your app."
        : "Nothing sent yet."));
      return;
    }
    const tb = h("tbody");
    for (const m of rows) {
      const who = direction === "in" ? (m.from.name ? `${m.from.name} <${m.from.address}>` : m.from.address) : m.to.map((t) => t.address).join(", ");
      tb.append(h("tr", {},
        h("td", {}, h("button", { class: "btn ghost sm", onclick: () => openMsg(m.id), "aria-label": `Open message: ${m.subject || "(no subject)"}` }, "Open")),
        h("td", { class: "mono" }, who),
        h("td", {}, (m.readAt || direction === "out" ? "" : "● ") + (m.subject || "(no subject)"), h("div", { class: "muted", style: "font-size:12px" }, m.snippet)),
        h("td", { class: "mono" }, m.local + (m.plusTag ? "+" + m.plusTag : "")),
        h("td", {}, direction === "in" ? (m.verified ? h("span", { class: "pill ok" }, "authenticated") : h("span", { class: "pill" }, "unverified")) : h("span", { class: "pill" }, m.status)),
        h("td", { class: "muted" }, fmtTime(m.receivedAt)),
      ));
    }
    listBox.append(h("div", { class: "tablewrap" }, h("table", {},
      h("thead", {}, h("tr", {},
        h("th", { scope: "col" }, ""), h("th", { scope: "col" }, direction === "in" ? "From" : "To"),
        h("th", { scope: "col" }, "Subject"), h("th", { scope: "col" }, "Name"),
        h("th", { scope: "col" }, direction === "in" ? "Sender" : "Status"), h("th", { scope: "col" }, "When"))),
      tb)));
    if (cursor) listBox.append(h("div", { class: "row", style: "margin-top:8px" }, more));
  }
  async function load(append) {
    try {
      const q = new URLSearchParams({ direction, limit: "25" });
      if (append && cursor) q.set("cursor", cursor);
      const res = await jGet(`${base}/messages?${q}`);
      if (append) rows = rows.concat(res.messages); else rows = res.messages;
      if (!append) cursor = res.cursor; else cursor = res.cursor;
      draw();
    } catch (e) { listBox.innerHTML = ""; listBox.append(errBox(e)); }
  }
  // Live refresh: only the first page, and only redrawn when something changed.
  async function poll() {
    try {
      const res = await jGet(`${base}/messages?direction=${direction}&limit=25`);
      const s = res.messages.map((m) => m.id + (m.readAt || "")).join(",");
      if (s !== sig && rows.length <= 25) { sig = s; rows = res.messages; cursor = res.cursor; draw(); }
    } catch {}
  }
  async function openMsg(id) {
    mbState.open = id;
    await renderMessage(view, id, direction, () => { load(false); });
    view.scrollIntoView({ behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  }
  body.append(listBox, view);
  load(false).then(() => { sig = rows.map((m) => m.id + (m.readAt || "")).join(","); });
  const timer = setInterval(poll, 3000);
  viewCleanup = () => clearInterval(timer);
  // A message another tab asked to show ("Open in inbox" after delivering one).
  const want = mbState.pendingOpen;
  mbState.pendingOpen = null;
  if (want) openMsg(want);
}

async function renderMessage(view, id, direction, reload) {
  const base = `/admin/mailbox/${mbState.instId}`;
  view.innerHTML = "";
  let res;
  try { res = await jGet(`${base}/messages/${encodeURIComponent(id)}`); }
  catch (e) { view.append(errBox(e)); return; }
  const m = res.message;
  const heading = h("h2", { tabindex: "-1" }, m.subject || "(no subject)");
  const bodyBox = h("div");
  const showText = () => { bodyBox.innerHTML = ""; bodyBox.append(h("pre", { style: "white-space:pre-wrap;max-height:420px" }, m.text != null ? m.text : "(no text part)")); };
  // HTML is rendered ONLY in a sandboxed frame: no script, no same-origin access, no forms.
  const showHTML = () => {
    bodyBox.innerHTML = "";
    const fr = h("iframe", { sandbox: "", title: "HTML body of this message", style: "width:100%;height:420px;border:1px solid var(--border);border-radius:8px;background:#fff" });
    fr.srcdoc = m.html || "";
    bodyBox.append(fr);
  };
  const actions = h("div", { class: "row", style: "margin:8px 0" });
  actions.append(h("button", { class: "btn ghost sm", onclick: showText }, "Text"));
  if (m.html != null) actions.append(h("button", { class: "btn ghost sm", onclick: showHTML }, "HTML (sandboxed)"));
  if (res.rawUrl) actions.append(h("a", { class: "btn ghost sm", href: res.rawUrl, download: "" }, "Download .eml"));
  if (direction === "in") {
    actions.append(h("button", { class: "btn sm", onclick: async () => {
      try {
        await jPost(`${base}/messages/${encodeURIComponent(id)}/replay`, {});
        toast("replayed through the current routes");
        renderMessage(view, id, direction, reload);
      } catch (e) { toast(e.message, true); }
    } }, "Replay through routes"));
  }
  actions.append(h("button", { class: "btn danger sm", onclick: async () => {
    if (!confirm("Delete this message and its attachments?")) return;
    try { await jDel(`${base}/messages/${encodeURIComponent(id)}`); toast("deleted"); view.innerHTML = ""; mbState.open = null; reload(); }
    catch (e) { toast(e.message, true); }
  } }, "Delete"));

  // Keep this one message longer or shorter than the mailbox's retention: 1–90 days from now.
  const expiresOut = h("span", {}, fmtTime(m.expiresAt));
  const keepSel = h("select", { id: nid("keep"), style: "width:auto" },
    ...[1, 7, 30, 90].map((d) => h("option", { value: String(d), ...(d === 30 ? { selected: "" } : {}) }, `${d} day${d === 1 ? "" : "s"} from now`)));
  const keepBtn = h("button", { class: "btn ghost sm", type: "button", onclick: async () => {
    keepBtn.disabled = true;
    try {
      const r = await jPost(`${base}/messages/${encodeURIComponent(id)}/keep`, { days: Number(keepSel.value) });
      m.expiresAt = r.message.expiresAt;
      expiresOut.textContent = fmtTime(m.expiresAt);
      toast(`kept until ${fmtTime(m.expiresAt)}`);
    } catch (e) { toast(e.message, true); } finally { keepBtn.disabled = false; }
  } }, "Change");
  const replyBox = h("div");
  if (direction === "in") {
    const replyBtn = h("button", { class: "btn sm", type: "button", "aria-expanded": "false", onclick: () => {
      if (replyBox.firstChild) { replyBox.innerHTML = ""; replyBtn.setAttribute("aria-expanded", "false"); replyBtn.focus(); return; }
      replyBtn.setAttribute("aria-expanded", "true");
      replyBox.append(mailSendForm(m, () => { replyBox.innerHTML = ""; replyBtn.setAttribute("aria-expanded", "false"); reload(); }));
      const first = replyBox.querySelector("textarea"); if (first) first.focus();
    } }, "Reply");
    actions.prepend(replyBtn);
  }

  const addr = (a) => (a.name ? `${a.name} <${a.address}>` : a.address);
  const card = h("div", { class: "card" }, heading,
    h("div", { class: "kv", style: "font-size:13px" },
      h("span", { class: "muted" }, "From"), h("span", { class: "mono" }, addr(m.from)),
      h("span", { class: "muted" }, "To"), h("span", { class: "mono" }, m.to.map(addr).join(", ") || "—"),
      ...(m.cc.length ? [h("span", { class: "muted" }, "Cc"), h("span", { class: "mono" }, m.cc.map(addr).join(", "))] : []),
      h("span", { class: "muted" }, "Name"), h("span", { class: "mono" }, m.local + (m.plusTag ? " (tag " + m.plusTag + ")" : "")),
      h("span", { class: "muted" }, "Sender"), h("span", {}, m.direction === "out" ? "this mailbox" : (m.verified ? "authenticated" : "not authenticated — reply mode will not answer it")),
      h("span", { class: "muted" }, "Status"), h("span", {}, m.status),
      h("span", { class: "muted" }, m.direction === "in" ? "Received" : "Sent"), h("span", {}, fmtTime(m.receivedAt)),
      h("label", { class: "muted", for: keepSel.id }, "Deleted after"), h("span", { class: "row", style: "gap:6px;flex-wrap:wrap" }, expiresOut, keepSel, keepBtn),
      ...(m.messageId ? [h("span", { class: "muted" }, "Message-ID"), h("span", { class: "mono" }, m.messageId)] : []),
    ),
    actions, replyBox, bodyBox);
  if ((m.textTruncated || m.htmlTruncated)) card.append(h("p", { class: "muted" }, "A body was cut at 512 KB; the full message is in the .eml."));

  if (m.attachments.length) {
    card.append(h("h2", {}, "Attachments"));
    const ul = h("ul");
    for (const a of m.attachments) {
      const name = a.filename || `attachment-${a.idx}`;
      ul.append(h("li", {},
        a.url ? h("a", { href: a.url, download: name }, name) : h("span", {}, name),
        h("span", { class: "muted" }, ` ${a.contentType}, ${fmtBytes(a.size)}`),
        a.rejected ? h("span", { class: "pill" }, `not stored: ${a.rejected}`) : null));
    }
    card.append(ul);
  }
  if (direction === "in") {
    card.append(h("h2", {}, "Route deliveries"));
    if (!m.deliveries.length) card.append(h("p", { class: "muted" }, "No route matched this message."));
    else {
      const tb = h("tbody");
      for (const d of m.deliveries) {
        const ok = d.status.startsWith("ok:");
        tb.append(h("tr", {},
          h("td", { class: "mono" }, d.action),
          h("td", {}, h("span", { class: "pill" + (ok ? " ok" : "") }, d.status)),
          h("td", {}, String(d.attempts)),
          h("td", { class: "muted" }, d.nextAt ? "retry " + fmtTime(d.nextAt) : "—")));
      }
      card.append(h("div", { class: "tablewrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", { scope: "col" }, "Action"), h("th", { scope: "col" }, "Result"), h("th", { scope: "col" }, "Attempts"), h("th", { scope: "col" }, "Next"))),
        tb)));
    }
  }
  view.append(card);
  showText();
  heading.focus();
}

// Send from this mailbox, or reply to `replyTo` — through the same path as /v1, so the send mode,
// the reply rule, the caps and the attachment limits apply. Nothing leaves the emulator.
// The whole message must stay under 5 MB once encoded, and base64 makes a file a third larger.
const MAIL_MAX_FILES = 32;
const MAIL_MAX_FILE_BYTES = Math.floor((5 * 1024 * 1024 - 64 * 1024) * 0.73);
function mailSendForm(replyTo, onSent) {
  const from = h("input", { value: replyTo ? replyTo.local : "inbox", autocomplete: "off" });
  const to = h("input", { value: replyTo ? (replyTo.replyTo[0] || replyTo.from).address : "", autocomplete: "off", placeholder: "a@example.com, b@example.com" });
  const cc = h("input", { autocomplete: "off", placeholder: "optional" });
  const subject = h("input", { value: replyTo ? (/^re:/i.test(replyTo.subject) ? replyTo.subject : "Re: " + replyTo.subject) : "" });
  const text = h("textarea", { placeholder: "Plain-text body" });
  const hintId = nid("files-hint");
  const files = h("input", { type: "file", multiple: "", "aria-describedby": hintId });
  const hint = h("div", { id: hintId, class: "muted", style: "font-size:12px" });
  const fileList = h("ul", { style: "margin:6px 0 0" });
  const picked = [];
  const result = h("div", { role: "status", "aria-live": "polite" });
  const send = h("button", { class: "btn", type: "submit" }, replyTo ? "Send reply" : "Send");
  const total = () => picked.reduce((n, f) => n + f.size, 0);
  function drawFiles() {
    const over = total() > MAIL_MAX_FILE_BYTES;
    hint.textContent = `${fmtBytes(total())} of ${fmtBytes(MAIL_MAX_FILE_BYTES)}` + (over ? " — too large to send" : "");
    hint.style.color = over ? "var(--err)" : "";
    files.setAttribute("aria-invalid", over ? "true" : "false");
    send.disabled = over;
    fileList.innerHTML = "";
    picked.forEach((f, i) => fileList.append(h("li", { class: "row", style: "gap:8px" },
      h("span", { class: "mono" }, f.filename, h("span", { class: "muted" }, ` (${fmtBytes(f.size)})`)),
      h("button", { class: "btn ghost sm", type: "button", "aria-label": `Remove ${f.filename}`, onclick: () => { picked.splice(i, 1); drawFiles(); files.focus(); } }, "Remove"))));
  }
  files.addEventListener("change", async () => {
    for (const f of files.files) {
      if (picked.length >= MAIL_MAX_FILES) { result.innerHTML = ""; result.append(h("p", { role: "alert" }, `At most ${MAIL_MAX_FILES} attachments.`)); break; }
      picked.push({ filename: f.name, contentType: f.type || "application/octet-stream", size: f.size, content: await fileToBase64(f) });
    }
    files.value = "";
    drawFiles();
  });
  const split = (s) => s.split(",").map((x) => x.trim()).filter(Boolean);
  const form = h("form", { class: "card", onsubmit: async (e) => {
    e.preventDefault();
    result.innerHTML = "";
    send.disabled = true;
    try {
      const body = { from: from.value.trim() || "inbox", to: split(to.value), cc: split(cc.value), subject: subject.value, text: text.value };
      if (picked.length) body.attachments = picked.map(({ filename, contentType, content }) => ({ filename, contentType, content }));
      const base = `/admin/mailbox/${mbState.instId}`;
      const res = replyTo
        ? await jPost(`${base}/messages/${encodeURIComponent(replyTo.id)}/reply`, body)
        : await jPost(`${base}/send`, body);
      toast("sent");
      onSent(res.message);
    } catch (err) {
      result.append(errBox(err));
    } finally { send.disabled = total() > MAIL_MAX_FILE_BYTES; }
  } },
    replyTo ? null : h("p", { class: "muted", style: "margin-top:0" }, "Sends through the same rules as the API. Nothing leaves your machine: with loopback on, mail to a local address arrives in that mailbox."),
    h("div", { class: "row" }, field("From (a name on this mailbox)", from), field("To", to)),
    field("Cc", cc, ""),
    field("Subject", subject, ""),
    field("Text", text, ""),
    field("Attachments", files, ""), hint, fileList,
    h("div", { class: "row", style: "margin-top:10px" }, send),
    result);
  drawFiles();
  return form;
}

// Compose a test email: a real MIME message, delivered through the inbound path.
function mailCompose(body, info) {
  const from = h("input", { value: "sender@example.com", autocomplete: "off" });
  const fromName = h("input", { placeholder: "optional" });
  const to = h("input", { value: info.address || "inbox", autocomplete: "off" });
  const subject = h("input", { value: "Test message" });
  const text = h("textarea", { placeholder: "Plain-text body" });
  const html = h("textarea", { placeholder: "<p>Optional HTML body</p>" });
  const files = h("input", { type: "file", multiple: "" });
  const auth = checkbox("Sender authenticated (lets reply mode answer this sender)", true);
  const result = h("div", { role: "status", "aria-live": "polite" });
  const send = h("button", { class: "btn", type: "submit" }, "Deliver");
  const form = h("form", { class: "card", onsubmit: async (e) => {
    e.preventDefault();
    result.innerHTML = "";
    send.disabled = true;
    try {
      const attachments = [];
      for (const f of files.files) attachments.push({ filename: f.name, contentType: f.type || "application/octet-stream", content: await fileToBase64(f) });
      const res = await jPost(`/admin/mailbox/${mbState.instId}/compose`, {
        from: from.value.trim(), fromName: fromName.value, to: to.value.trim(), subject: subject.value,
        text: text.value, html: html.value, attachments, authenticated: auth.cb.checked,
      });
      toast("delivered");
      result.append(h("p", {}, "Delivered as ", h("span", { class: "mono" }, res.message.id), ". ",
        h("button", { class: "btn ghost sm", type: "button", onclick: () => { mbState.pendingOpen = res.message.id; openTab("Inbox"); } }, "Open in inbox")));
    } catch (err) {
      result.append(refusalBox(err));
    } finally { send.disabled = false; }
  } },
    h("div", { class: "row" }, field("From", from), field("From name", fromName)),
    h("div", { class: "row" }, field("To (a full address, or a bare name on this mailbox)", to)),
    field("Subject", subject, ""),
    field("Text", text, ""),
    field("HTML", html, ""),
    field("Attachments", files, ""),
    h("div", { style: "margin-top:8px" }, auth.el),
    h("div", { class: "row", style: "margin-top:10px" }, send),
    result);
  body.append(form);
}

// A refusal shows the SMTP answer a real sender would have received.
function refusalBox(err) {
  if (err.status === 422 && err.data && err.data.rejected) {
    return h("div", { class: "card", role: "alert", style: "border-color:var(--err)" },
      h("strong", { style: "color:var(--err)" }, "Refused"),
      h("pre", { style: "margin:6px 0 0" }, err.data.rejected));
  }
  return errBox(err);
}

function openTab(name) {
  const holder = $("#mb-holder"); if (!holder) return;
  for (const b of holder.querySelectorAll(".subtab")) if (b.textContent === name) { b.click(); return; }
}

function mailUpload(body, info) {
  const file = h("input", { type: "file", accept: ".eml,message/rfc822" });
  const to = h("input", { value: info.address || "inbox" });
  const from = h("input", { value: "sender@example.com" });
  const auth = checkbox("Sender authenticated", true);
  const result = h("div", { role: "status", "aria-live": "polite" });
  const go = h("button", { class: "btn", type: "submit" }, "Deliver .eml");
  body.append(h("form", { class: "card", onsubmit: async (e) => {
    e.preventDefault();
    result.innerHTML = "";
    if (!file.files.length) { result.append(h("p", { role: "alert" }, "Choose an .eml file first.")); return; }
    const q = new URLSearchParams({ to: to.value.trim(), from: from.value.trim(), authenticated: auth.cb.checked ? "1" : "0" });
    try {
      const res = await fetch(`/admin/mailbox/${mbState.instId}/eml?${q}`, { method: "POST", body: file.files[0] });
      const data = await res.json().catch(() => null);
      if (!res.ok) { const err = new Error(data && data.error ? `${data.error.code}: ${data.error.message}` : `HTTP ${res.status}`); err.status = res.status; err.data = data; throw err; }
      toast("delivered");
      result.append(h("p", {}, "Delivered as ", h("span", { class: "mono" }, data.message.id), ". ",
        h("button", { class: "btn ghost sm", type: "button", onclick: () => { mbState.pendingOpen = data.message.id; openTab("Inbox"); } }, "Open in inbox")));
    } catch (err) { result.append(refusalBox(err)); }
  } },
    h("p", { class: "muted", style: "margin-top:0" }, "Delivers the file unmodified — malformed messages included — through the same path as any inbound mail."),
    field(".eml file", file, ""),
    h("div", { class: "row" }, field("Envelope to", to), field("Envelope from (used when the message has no From)", from)),
    h("div", { style: "margin-top:8px" }, auth.el),
    h("div", { class: "row", style: "margin-top:10px" }, go),
    result));
}

// Settings and the routes editor. The save is a FULL replace, so every field is named.
function mailSettings(body, info) {
  const s = info.config.settings;
  const sel = (opts, v) => h("select", {}, ...opts.map((o) => h("option", { value: o, ...(o === v ? { selected: "" } : {}) }, o)));
  const num = (v, ph) => h("input", { type: "number", min: "0", value: v == null ? "" : String(v), placeholder: ph || "" });
  const acceptMode = sel(["any", "registered"], s.acceptMode);
  const sendMode = sel(["off", "reply", "any"], s.sendMode);
  const retention = num(s.retentionDays);
  const inboundCap = num(s.inboundDailyCap);
  const sendCap = num(s.dailySendCap, "organization cap");
  const rateLimit = num(s.rateLimit, "no limit");
  const keepRaw = checkbox("Keep the raw .eml", s.keepRaw);
  const optNum = (el) => (el.value.trim() === "" ? null : Number(el.value));

  const routes = JSON.parse(JSON.stringify(info.config.routes || []));
  const routesBox = h("div");
  function drawRoutes() {
    routesBox.innerHTML = "";
    if (!routes.length) routesBox.append(h("p", { class: "muted" }, "No routes: mail is stored and nothing is called."));
    routes.forEach((r, i) => {
      const m = r.match || (r.match = {});
      const inp = (key, ph) => { const el = h("input", { value: m[key] || "", placeholder: ph }); el.addEventListener("input", () => { if (el.value) m[key] = el.value; else delete m[key]; }); return el; };
      const hasAtt = sel(["either", "yes", "no"], m.hasAttachment === true ? "yes" : m.hasAttachment === false ? "no" : "either");
      hasAtt.addEventListener("change", () => { if (hasAtt.value === "either") delete m.hasAttachment; else m.hasAttachment = hasAtt.value === "yes"; });
      const acts = h("div");
      (r.do || (r.do = [])).forEach((a, j) => {
        const kind = a.function != null ? "function" : a.webhook != null ? "webhook" : "channel";
        const kindSel = sel(["function", "channel", "webhook"], kind);
        const target = h("input", { value: a[kind] || "", placeholder: kind === "webhook" ? "https://example.com/hook" : "<instance>/<name>" });
        const incl = checkbox("include text", !!a.includeText);
        const sync = () => {
          const k = kindSel.value;
          r.do[j] = { [k]: target.value.trim() };
          if (k === "channel" && incl.cb.checked) r.do[j].includeText = true;
          incl.el.style.display = k === "channel" ? "" : "none";
          target.placeholder = k === "webhook" ? "https://example.com/hook" : "<instance>/<name>";
        };
        kindSel.addEventListener("change", sync); target.addEventListener("input", sync); incl.cb.addEventListener("change", sync);
        incl.el.style.display = kind === "channel" ? "" : "none";
        acts.append(h("div", { class: "row", style: "margin:4px 0" },
          field(`Action ${j + 1} kind`, kindSel, ""), field(`Action ${j + 1} target`, target), h("div", { style: "align-self:end" }, incl.el),
          h("button", { class: "btn danger sm", type: "button", style: "align-self:end", onclick: () => { r.do.splice(j, 1); drawRoutes(); } }, `Remove action ${j + 1}`)));
      });
      routesBox.append(h("fieldset", { class: "card" },
        h("legend", {}, `Route ${i + 1}${i === 0 ? " (checked first)" : ""}`),
        h("div", { class: "row" }, field("To name (glob)", inp("to", "e.g. support*")), field("From (glob)", inp("from", "e.g. *@github.com"))),
        h("div", { class: "row" }, field("Subject contains", inp("subject", "")), field("Has attachment", hasAtt)),
        acts,
        h("div", { class: "row", style: "margin-top:6px" },
          h("button", { class: "btn ghost sm", type: "button", onclick: () => { if (r.do.length >= 3) { toast("at most 3 actions per route", true); return; } r.do.push({ function: "" }); drawRoutes(); } }, "+ Action"),
          h("button", { class: "btn ghost sm", type: "button", disabled: i === 0 ? "" : null, onclick: () => { [routes[i - 1], routes[i]] = [routes[i], routes[i - 1]]; drawRoutes(); } }, "Move up"),
          h("button", { class: "btn danger sm", type: "button", onclick: () => { routes.splice(i, 1); drawRoutes(); } }, `Remove route ${i + 1}`))));
    });
  }
  drawRoutes();
  const result = h("div", { role: "status", "aria-live": "polite" });
  const save = h("button", { class: "btn", type: "submit" }, "Save settings and routes");
  body.append(h("form", { onsubmit: async (e) => {
    e.preventDefault();
    result.innerHTML = "";
    const config = {
      settings: {
        rateLimit: optNum(rateLimit), region: s.region, acceptMode: acceptMode.value, inboundDailyCap: optNum(inboundCap),
        retentionDays: optNum(retention), sendMode: sendMode.value, dailySendCap: optNum(sendCap), keepRaw: keepRaw.cb.checked,
      },
      routes,
    };
    try {
      await jPut(`/admin/mailbox/${mbState.instId}/config`, { config });
      toast("saved");
      result.append(h("p", {}, "Saved. Replay a message to run it through the new routes."));
    } catch (err) { result.append(h("div", { role: "alert" }, errBox(err))); }
  } },
    h("div", { class: "card" }, h("h2", { style: "margin-top:0" }, "Settings"),
      h("div", { class: "row" }, field("Accept mail for", acceptMode), field("Send mode", sendMode), field("Retention (days, 1–90)", retention)),
      h("div", { class: "row" }, field("Inbound per day (max 5000)", inboundCap), field("Sends per day (blank: org cap)", sendCap), field("Rate limit (req/min, blank: none)", rateLimit)),
      h("div", { style: "margin-top:8px" }, keepRaw.el),
      h("p", { class: "muted", style: "font-size:12px" }, `Region: ${s.region} (fixed at creation).`)),
    h("h2", {}, "Routes"),
    h("p", { class: "muted" }, "The first route whose every condition holds runs its actions."),
    routesBox,
    h("div", { class: "row", style: "margin:8px 0" },
      h("button", { class: "btn ghost", type: "button", onclick: () => { if (routes.length >= 20) { toast("at most 20 routes", true); return; } routes.push({ match: {}, do: [{ function: "" }] }); drawRoutes(); } }, "+ Route"),
      save),
    result));

  // The webhook signing secret.
  const secretOut = h("pre", { "aria-live": "polite" }, "hidden");
  body.append(h("div", { class: "card" }, h("h2", { style: "margin-top:0" }, "Webhook signing secret"),
    h("p", { class: "muted", style: "font-size:12px" }, "Webhook deliveries carry x-altengine-timestamp and x-altengine-signature: v1=<hex HMAC-SHA256 of \"<timestamp>.<body>\">."),
    secretOut,
    h("div", { class: "row" },
      h("button", { class: "btn ghost", onclick: async () => { try { secretOut.textContent = (await jPost(`/admin/mailbox/${mbState.instId}/webhook-secret/reveal`, {})).secret; } catch (e) { toast(e.message, true); } } }, "Reveal"),
      h("button", { class: "btn danger", onclick: async () => {
        if (!confirm("Rotate the secret? Receivers checking the old one will reject every delivery.")) return;
        try { secretOut.textContent = (await jPost(`/admin/mailbox/${mbState.instId}/webhook-secret/rotate`, {})).secret; toast("rotated"); } catch (e) { toast(e.message, true); }
      } }, "Rotate"))));
}

function mailAddresses(body, info) {
  const base = `/admin/mailbox/${mbState.instId}`;
  const list = h("div");
  const local = h("input", { placeholder: "blank mints one" });
  const label = h("input", { placeholder: "optional" });
  async function load() {
    list.innerHTML = "";
    try {
      const { addresses, cursor } = await jGet(`${base}/addresses?limit=100`);
      if (!addresses.length) { list.append(h("div", { class: "empty" }, "No registered or disabled addresses. Any name receives mail unless the mailbox accepts registered names only.")); return; }
      const tb = h("tbody");
      for (const a of addresses) {
        tb.append(h("tr", {},
          h("td", { class: "mono" }, a.address || a.local),
          h("td", {}, a.label || "—"),
          h("td", {}, a.disabledAt ? "disabled" : a.registered ? "registered" : "—"),
          h("td", {}, a.disabledAt ? null : h("button", { class: "btn danger sm", onclick: async () => {
            try { await jDel(`${base}/addresses/${encodeURIComponent(a.local)}`); toast("disabled"); load(); } catch (e) { toast(e.message, true); }
          } }, `Disable ${a.local}`))));
      }
      list.append(h("div", { class: "tablewrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", { scope: "col" }, "Address"), h("th", { scope: "col" }, "Label"), h("th", { scope: "col" }, "State"), h("th", { scope: "col" }, ""))), tb)));
      if (cursor) list.append(h("p", { class: "muted" }, "First 100 shown."));
    } catch (e) { list.append(errBox(e)); }
  }
  body.append(h("form", { class: "card", onsubmit: async (e) => {
    e.preventDefault();
    try {
      const rec = await jPost(`${base}/addresses`, { local: local.value.trim() || null, label: label.value || null });
      toast("registered " + (rec.address || rec.local)); local.value = ""; label.value = ""; load();
    } catch (err) { toast(err.message, true); }
  } }, h("div", { class: "row" }, field("Name", local), field("Label", label), h("div", { style: "align-self:end" }, h("button", { class: "btn", type: "submit" }, "Register")))), list);
  load();
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
