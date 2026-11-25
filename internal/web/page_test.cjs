const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(path.join(__dirname, "page.js"), "utf8");
// Load the actual pure functions without starting the DOM or polling timers.
function context(names, globals) {
  const code = names.map((name) => {
    const match = source.match(new RegExp("  function " + name + "\\([^]*?\\n  }"));
    assert.ok(match, name);
    return match[0];
  }).join("\n");
  const ctx = vm.createContext(globals || {});
  vm.runInContext(code, ctx);
  return ctx;
}

test("a save counts as applied only by its own config token or a later one", () => {
  const ctx = context(["tokenApplied", "checkApplied"], { status: { config: { bark_url: "set" } } });
  const tokens = ["a1", "b2", "c3"];
  assert.equal(ctx.tokenApplied(tokens, "b2", "a1"), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", "b2"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", "c3"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", ""), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", undefined), false);
  // A token saved elsewhere, or a redacted value in the status, proves nothing.
  assert.equal(ctx.tokenApplied(tokens, "b2", "other"), false);
  assert.equal(ctx.tokenApplied(tokens, "zz", "c3"), false);

  const settled = [];
  ctx.savedTokens = tokens;
  const wait = (token) => {
    const w = { token, done: (v) => { settled.push([token, v]); ctx.applyWaits = ctx.applyWaits.filter((x) => x !== w); } };
    return w;
  };
  ctx.applyWaits = [wait("b2"), wait("c3")];
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.deepEqual(settled, [["b2", true]]);
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "c3";
  ctx.checkApplied();
  assert.deepEqual(settled.at(-1), ["c3", true]);
  assert.equal(ctx.applyWaits.length, 0);
  assert.doesNotMatch(source, /function sameConfig|mcp_key_set/);
});

test("config tokens are 16 hexadecimal digits", () => {
  const ctx = context(["newConfigToken"], { crypto: require("node:crypto").webcrypto, Uint8Array });
  const a = ctx.newConfigToken(), b = ctx.newConfigToken();
  assert.match(a, /^[0-9a-f]{16}$/);
  assert.notEqual(a, b);
});

test("account names preserve trailing note lines and clearing removes the whole note", () => {
  const ctx = context(["accountNote"]);
  assert.equal(ctx.accountNote("old\r\nsecond\r\nthird", "new"), "new\nsecond\nthird");
  assert.equal(ctx.accountNote("old\nsecond", ""), "");
  assert.equal(ctx.accountNote("", "new"), "new");
});

test("drag targets use layout midpoints rather than animated rectangles", () => {
  const ctx = context(["accountDropIndex"]);
  const rows = [0, 100, 200].map((offsetTop) => ({
    offsetTop, offsetHeight: 80,
    getBoundingClientRect: () => { throw new Error("animated geometry must not be read"); },
  }));
  assert.equal(ctx.accountDropIndex(rows, 49, 10), 0);
  assert.equal(ctx.accountDropIndex(rows, 51, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 150, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 151, 10), 2);
  assert.equal(ctx.accountDropIndex(rows, 999, 10), 3);
});

test("queued inline weights merge changes and an empty field removes only its own override", async () => {
  const states = [], writes = [];
  const ctx = context(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight"], {
    rawConfig: {}, saveQueue: Promise.resolve(), history: null,
    saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
    translate: (key) => key, setSaveState: (...args) => states.push(args),
    claimState: () => () => {}, showApplied: () => {},
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, renderAccounts: () => {},
  });
  const input = (value) => ({ value, parentNode: { firstChild: {} }, accountSaved: "", accountEdit: 1, accountDirty: true, validity: { badInput: false }, hasAttribute: () => false, getAttribute: () => "weight" });
  const a = input("2.5"), b = input("7");
  ctx.saveAccountField(a, { account: { name: "a" } });
  ctx.saveAccountField(b, { account: { name: "b" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { a: 2.5, b: 7 });
  a.value = ""; a.accountEdit++;
  ctx.saveAccountField(a, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { b: 7 });
  for (const bad of ["0", "-1", "Infinity", "NaN"]) {
    ctx.saveAccountField(input(bad), { account: { name: "a" } });
  }
  const invalidEmpty = input(""); invalidEmpty.validity.badInput = true;
  ctx.saveAccountField(invalidEmpty, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.equal(writes.length, 3);
  assert.equal(states.length, 5);
});

test("an earlier account name save does not overwrite a newer unsaved edit", async () => {
  const ctx = context(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight"], {
    saveQueue: Promise.resolve(), history: null, saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
    claimState: () => () => {}, translate: (key) => key, renderAccounts: () => {},
    saveAccountName: async () => {},
  });
  const input = { value: "first", parentNode: { firstChild: {} }, accountSaved: "old", accountEdit: 1, accountDirty: true, getAttribute: () => "name" };
  ctx.saveAccountField(input, { account: { name: "a", note: "old\nrest" } });
  input.value = "new unsaved draft"; input.accountEdit++;
  await ctx.saveQueue;
  assert.equal(input.value, "new unsaved draft");
  assert.equal(input.accountDirty, true);
  assert.equal(input.accountPending, 0);
});

// Minimal DOM doubles exercise the actual state aggregation and save queue.
function saveNode(tagName, attrs = {}) {
  return {
    tagName, id: attrs.id || "", name: attrs.name || "", children: [], attrs: { ...attrs },
    value: "", validity: { badInput: false }, accountSaved: "", accountEdit: 1, accountDirty: true,
    get textContent() { return (this.text || "") + this.children.map(n => n.textContent).join(""); },
    set textContent(value) { this.text = value; this.children = []; },
    getAttribute(name) { return this.attrs[name] ?? null; },
    hasAttribute(name) { return name in this.attrs; },
    setAttribute(name, value) { this.attrs[name] = value; },
    removeAttribute(name) { delete this.attrs[name]; },
    appendChild(node) { this.children.push(node); return node; },
    querySelector(selector) { return selector === ".save-state" ? this.children[0] : null; },
    closest() { return null; },
  };
}

const saveFunctions = ["saveHost", "saveControls", "saveItemKey", "saveEntry", "renderSaveState",
  "updateSaveEntry", "setSaveState", "claimState", "editSaveState"];
function stateContext(names = [], globals = {}) {
  return context([...saveFunctions, ...names], {
    saveSerial: 0,
    translate: (key, ...args) => {
      if (key === "save_field_failed") return "✕ " + args[0] + " save failed";
      if (key === "save_failure_reason") return ": " + args[0];
      if (key === "save_failed_compact") return "✕ Save failed";
      if (key === "save_more_failed") return " (" + args[0] + " more failed)";
      return key;
    },
    saveItemLabel: input => input.id === "account-order-state" ? "Order" : input.getAttribute("data-account-field") || input.name,
    el: (tag, attrs) => saveNode(tag.toUpperCase(), attrs),
    clear: node => { node.textContent = ""; },
    maskedNodes: text => [{ textContent: text }],
    ...globals,
  });
}

function accountSaveContext(fail) {
  const heading = saveNode("H2", { id: "account-order-state" });
  const section = { tagName: "SECTION", querySelector: selector => { assert.equal(selector, ".card-title"); return heading; } };
  const row = { account: { name: "a", note: "old" }, getAttribute: () => "a" };
  const ctx = stateContext(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight", "saveAccountOrder"], {
    fail, rawConfig: {}, saveQueue: Promise.resolve(), history: null, pendingOrder: null,
    $: () => heading, toast: () => {},
    showApplied: say => { say("waiting", "waiting"); say("saved", "applied"); },
    saveAccountName: async () => { if (ctx.fail === "name" || ctx.fail === "unauthorized") throw new Error(ctx.fail); },
    patchConfig: async patch => {
      if (ctx.fail === "weight" && patch.account_weights || ctx.fail === "order" && patch.account_order) throw new Error("failed");
      Object.assign(ctx.rawConfig, patch);
      return "token";
    },
    loadConfig: async () => {}, sortAccounts: () => {}, renderAccounts: () => {},
  });
  const field = (kind, value) => {
    const input = saveNode("INPUT", { "data-account-field": kind });
    input.value = value;
    input.closest = selector => selector === ".account-setting" ? row : selector === ".settings-section" ? section : null;
    return input;
  };
  return { ctx, heading, row, field, name: field("name", "unsaved name"), weight: field("weight", "2.5") };
}

test("a failed name save stays in the heading after successful weight and order saves", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("name");
  ctx.saveAccountField(name, row);
  ctx.saveAccountField(weight, row);
  ctx.saveAccountOrder(["a"]);
  await ctx.saveQueue;
  assert.equal(heading.children.length, 1);
  assert.equal(heading.children[0].getAttribute("data-state"), "failed");
  assert.match(heading.textContent, /name save failed/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(name.value, "unsaved name");
  assert.equal(name.accountDirty, true);
  assert.equal(name.accountPending, 0);
  assert.equal(weight.accountDirty, false);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.doesNotMatch(heading.textContent, /failed/);
});

test("weight and order failures survive a successful name save", async () => {
  for (const fail of ["weight", "order"]) {
    const { ctx, heading, row, name, weight } = accountSaveContext(fail);
    if (fail === "weight") ctx.saveAccountField(weight, row);
    else ctx.saveAccountOrder(["a"]);
    ctx.saveAccountField(name, row);
    await ctx.saveQueue;
    assert.equal(heading.children[0].getAttribute("data-state"), "failed");
    assert.match(heading.textContent, fail === "weight" ? /weight save failed/ : /Order save failed/);
    if (fail === "weight") {
      assert.equal(weight.getAttribute("aria-invalid"), "true");
      assert.equal(weight.value, "2.5");
      assert.equal(weight.accountDirty, true);
    }
  }
});

test("reverting a failed account field to its saved value still retries and clears the failure", async () => {
  const { ctx, heading, row, weight } = accountSaveContext("weight");
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  weight.value = weight.accountSaved;
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.equal(ctx.rawConfig.account_weights, null);
});

test("multiple failures show the latest reason and count, with every failed control marked", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("unauthorized");
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  weight.value = "0";
  ctx.saveAccountField(weight, row);
  assert.match(heading.textContent, /weight save failed: weight_invalid \(1 more failed\)/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, name);
  assert.match(heading.textContent, /1 more failed/);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  assert.doesNotMatch(heading.textContent, /more failed/);
  weight.value = "3";
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
});

test("all settings resolve to section headings or their own advanced legends", () => {
  const ctx = stateContext();
  for (const advanced of [false, true]) {
    for (const name of ["bark_url", "ignition.start_hour", "providers.codex.monitor", "remember_chart_filters", "mcp_enabled"]) {
      const heading = saveNode(advanced ? "LEGEND" : "H2");
      const group = { tagName: advanced ? "FIELDSET" : "SECTION", querySelector: selector => {
        assert.equal(selector, advanced ? "legend" : ".card-title"); return heading;
      } };
      const input = saveNode("INPUT", { name });
      input.closest = selector => selector === (advanced ? "fieldset" : ".settings-section") ? group : null;
      assert.equal(ctx.saveHost(input), heading);
      ctx.setSaveState(heading, "failed", "reason", input);
      assert.equal(heading.children.length, 1);
      assert.equal(heading.children[0].attrs.class, "save-state");
      assert.equal(input.children.length, 0);
      assert.equal(input.getAttribute("aria-invalid"), "true");
    }
  }
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /<form id="settings">\s*<section[^>]+>\s*<h2[^>]+id="account-order-state"/);
  assert.doesNotMatch(html, /data-save-in/);
});

test("ordinary settings keep A's failure through B's save and reject stale replies after editing", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a);
  sayA("failed", "A failed");
  const sayB = ctx.claimState(heading, b);
  sayB("saved", "B saved");
  assert.match(heading.textContent, /a save failed: A failed/);
  assert.equal(a.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, a);
  assert.equal(sayA("saved", "late reply"), false);
  assert.match(heading.textContent, /A failed/);
  const retry = ctx.claimState(heading, a);
  assert.match(heading.textContent, /A failed/);
  retry("waiting", "saved, waiting");
  assert.equal(a.hasAttribute("aria-invalid"), false);
  retry("saved", "applied");
  assert.equal(heading.textContent, "applied");
});

test("an older confirmation cannot replace a more recent save's progress", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a), sayB = ctx.claimState(heading, b);
  sayB("waiting", "B waiting");
  sayA("saved", "A applied");
  assert.equal(heading.textContent, "B waiting");
});

test("account cards keep only identity in the heading and place actions after the footer plan", () => {
  const cards = saveNode("DIV");
  const element = (tag, attrs = {}, children = []) => {
    const node = saveNode(tag.toUpperCase(), attrs);
    if (attrs.text) node.textContent = attrs.text;
    children.filter(Boolean).forEach(child => node.appendChild(child));
    return node;
  };
  const ctx = context(["renderAccounts"], {
    accountDrag: null, accountsBehind: false, SENSITIVE: "account-sensitive", document: { activeElement: null },
    status: { accounts: [
      { name: "a", provider: "codex", title: "GPT#first", email: "first@example.com", plan: "Pro", groups: [] },
      { name: "b", provider: "codex", title: "GPT#second", email: "second@example.com", groups: [] },
    ] },
    rawConfig: {}, masked: false, renderAccountSettings: () => {}, accountWeight: () => 2,
    fmtWeight: String, parseTime: () => null, $: () => cards,
    clear: node => { node.textContent = ""; }, el: element,
    translate: (key, value) => value === undefined ? key : key + ": " + value,
    maskedNodes: text => [element("span", { class: "account-sensitive", text })],
    snapshotButton: label => element("button", { class: "snapshot no-snapshot", "aria-label": label }),
  });
  ctx.renderAccounts();
  assert.equal(cards.children.length, 2);
  cards.children.forEach((card, i) => {
    const heading = card.children[0], footer = card.children.at(-1);
    assert.equal(heading.children.length, 2);
    assert.ok(heading.children.every(node => node.tagName === "SPAN"));
    assert.match(heading.children[0].attrs.class, /type-badge/);
    assert.match(heading.children[1].attrs.class, /account-email account-sensitive/);
    assert.match(heading.children[0].children.at(-1).attrs.class, /account-weight/);
    assert.equal(footer.attrs.class, "account-head cell-note");
    assert.equal(footer.children[0].textContent, i ? "" : "plan: Pro");
    assert.equal(footer.children[1].textContent, "refresh");
    assert.equal(footer.children[2].attrs["aria-label"], "snapshot_card");
    footer.children.slice(1).forEach(button => {
      assert.equal(button.tagName, "BUTTON");
      assert.match(button.attrs.class, /no-snapshot/);
    });
  });
  assert.match(source, /copy.querySelectorAll\("\.no-snapshot,/);
});

function settingsSaveContext(radio = false) {
  const a = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.start_hour" });
  const b = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.end_hour" });
  const inputs = [a, b], applied = [], refilled = [];
  inputs.forEach((input, i) => { input.type = "number"; input.value = String(i ? 22 : 7); input.checkValidity = () => true; });
  if (radio) inputs.forEach((input, i) => {
    input.type = "radio"; input.value = i ? "all" : "off"; input.checked = !i;
    input.closest = selector => selector.includes(".mode-switch") ? { querySelectorAll: () => inputs } : null;
  });
  const ctx = context(["saveField", "captureIntent", "buildPatch", "refillSaved", "noteEdit", "inputTop", "topKey", "setPath", "saveControls"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig: { ignition: {} }, status: { config: {} },
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async patch => { Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {},
    showApplied: () => new Promise(resolve => applied.push(resolve)),
    fillSettings: keys => refilled.push(plain(keys)), renderIgnitionOff: () => {},
  });
  const edit = (input, value) => { input.value = value; input.settingsDirty = true; ctx.noteEdit(input); };
  return { ctx, a, b, applied, refilled, edit };
}

test("consecutive saves within one top-level key clear both submitted drafts independently", async () => {
  const { ctx, a, b, applied, refilled, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  assert.equal(applied.length, 2);
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(refilled.length, 2);
});

test("confirmation preserves newer drafts and independently failed controls of the same key", async () => {
  const { ctx, a, b, applied, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  edit(a, "9");
  applied[0](true); applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(a.value, "9");
  assert.equal(b.settingsDirty, false);
  b.settingsDirty = true;
  b.setAttribute("aria-invalid", "true");
  edit(a, "10"); ctx.saveField({ target: a });
  await ctx.saveQueue;
  applied[2](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  assert.equal(b.getAttribute("aria-invalid"), "true");
});

test("a radio selection owns the generations and submitted state of the whole group", async () => {
  const { ctx, a, b, applied } = settingsSaveContext(true);
  a.settingsDirty = true; ctx.noteEdit(a); ctx.saveField({ target: a });
  a.checked = false; b.checked = true;
  b.settingsDirty = true; ctx.noteEdit(b); ctx.saveField({ target: b });
  await ctx.saveQueue;
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(a.checked, false);
  assert.equal(b.checked, true);
});

test("fields skipped by patch construction keep their drafts after confirmation", () => {
  const { ctx, a, b, edit } = settingsSaveContext();
  edit(a, ""); edit(b, "23");
  const intent = ctx.captureIntent(["ignition"]);
  ctx.buildPatch("ignition", intent);
  assert.equal(intent.submitted.includes(a), false);
  ctx.refillSaved(intent);
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, false);
});

test("recreated account inputs inherit drafts and failures and receive later save results", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext("name");
  ctx.saveAccountField(name, row); await ctx.saveQueue;
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  const entry = heading.saveEntries.get("a:name");
  assert.equal(entry.input, replacement);
  assert.equal(entry.controls[0], replacement);
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.hasAttribute("aria-invalid"), false);
  assert.equal(replacement.accountDirty, false);
});

test("pending account saves settle against the current binding after the row is recreated", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext();
  let finish;
  ctx.saveAccountName = () => new Promise(resolve => { finish = resolve; });
  ctx.saveAccountField(name, row);
  await Promise.resolve();
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  assert.equal(replacement.accountPending, 1);
  finish(); await ctx.saveQueue;
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.accountPending, 0);
  assert.equal(replacement.accountDirty, false);
});

test("failed states separate the compact summary, field summary, reason and count", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  ctx.setSaveState(heading, "failed", "first reason", saveNode("INPUT", { name: "first" }));
  ctx.setSaveState(heading, "failed", "second reason", saveNode("INPUT", { name: "long field name" }));
  const parts = Object.fromEntries(heading.children[0].children.map(n => [n.attrs["data-save-part"], n.textContent]));
  assert.equal(parts.compact, "✕ Save failed");
  assert.equal(parts.summary, "✕ long field name save failed");
  assert.equal(parts.reason, ": second reason");
  assert.equal(parts.count, " (1 more failed)");
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8");
  assert.match(css, /@media \(max-width: 640px\)/);
  assert.match(css, /\.save-state\[data-state="failed"\] > \[data-save-part="summary"\], \.save-state > \[data-save-part="reason"\] { display: none; }/);
});

test("English text interpolates values and reads provider labels", () => {
  const code = source.slice(source.indexOf("  var I18N ="), source.indexOf("\n  function translate("));
  const globals = vm.createContext({});
  vm.runInContext(code, globals);
  assert.deepEqual(Object.keys(globals.I18N), ["en"]);
  const ctx = context(["translate", "localized", "listText"], globals);
  assert.equal(ctx.translate("save_applied"), "In effect");
  assert.equal(ctx.translate("mcp_error", "bind failed"), "The MCP endpoint is not running: bind failed");
  assert.equal(ctx.translate("test_failed", "Webhook", "HTTP 500"), "Webhook: failed, HTTP 500");
  assert.equal(ctx.translate("unknown_key"), "unknown_key");
  assert.equal(ctx.localized({ en: "café quota" }), "café quota");
  assert.equal(ctx.localized("naïve résumé 🙂"), "naïve résumé 🙂");
  assert.equal(ctx.localized(null), "");
  assert.equal(ctx.localized({}), "");
  assert.equal(ctx.listText(["A", "B", "C"]), "A, B or C");
});

const HOUR = 3600, DAY = 86400, WEEK = 7 * DAY;
function pace() { return context(["paceSegments", "paceAt", "pooledPace"], { RESET_DRIFT: 60 }); }
// Arrays from the VM context have another prototype.
const plain = (value) => JSON.parse(JSON.stringify(value));

test("an early reset cuts the even pace where the new reset appears", () => {
  const ctx = pace();
  const t0 = 1000 * DAY, x1 = t0 + 5 * DAY, early = t0 + 3 * DAY, x2 = early + WEEK;
  const segs = ctx.paceSegments([[t0 - 3 * DAY, x1], [early, x2]], WEEK, x2, early + DAY);
  assert.equal(segs.length, 2);
  // The old line starts where its window began and stops at the early reset
  // at the share an even pace leaves there, not at 0%.
  assert.deepEqual(plain(segs[0]), [x1 - WEEK, early, x1, false]);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early - 1) - 100 * 2 / 7) < 0.01);
  // From the early reset on, the new window runs from 100% over a full week.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early) - 100) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early + DAY) - 100 * 6 / 7) < 1e-9);
  assert.equal(ctx.paceAt(segs, WEEK, x2 + 1), null);
});

test("reset times that drift by seconds keep one continuous line", () => {
  const ctx = pace();
  const x = 2000 * DAY, t0 = x - 4 * HOUR, t1 = x - 3 * HOUR;
  const segs = ctx.paceSegments([[t0, x], [t1, x + 2]], 5 * HOUR, x + 30, t1 + 600);
  // The status reset within RESET_DRIFT of the history adds no segment.
  assert.equal(segs.length, 2);
  const before = ctx.paceAt(segs, 5 * HOUR, t1 - 1e-6), after = ctx.paceAt(segs, 5 * HOUR, t1);
  assert.ok(Math.abs(before - after) < 0.05, before + " " + after);
  assert.ok(Math.abs(after - 60) < 0.05);
});

test("no even pace while the reset is unknown, and a new status reset applies from now", () => {
  const ctx = pace();
  const L = 5 * HOUR, x = 3000 * DAY, now = x + 2 * HOUR;
  const segs = ctx.paceSegments([[x - 4 * HOUR, x], [x - HOUR, 0]], L, x + 4 * HOUR, now);
  assert.equal(ctx.paceAt(segs, L, x - 2 * HOUR) > 0, true);
  assert.equal(ctx.paceAt(segs, L, x - 30 * 60), null);
  // From the moment the reset is unknown, not only after it.
  assert.equal(ctx.paceAt(segs, L, x - HOUR), null);
  assert.ok(ctx.paceAt(segs, L, x - HOUR - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now - 60), null);
  assert.ok(Math.abs(ctx.paceAt(segs, L, now) - 40) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, L, x + 3 * HOUR) - 20) < 1e-9);
  assert.deepEqual(plain(ctx.paceSegments([], L, 0, now)), []);
  assert.deepEqual(plain(ctx.paceSegments(undefined, L, null, now)), []);
  assert.deepEqual(plain(ctx.paceSegments([[now - HOUR, now + HOUR]], 0, null, now)), []);
});

test("a total has an even pace only while every account has one", () => {
  const ctx = pace();
  const L = WEEK, now = 4000 * DAY;
  const a = { weight: 1, segments: ctx.paceSegments([[now - 2 * DAY, now + 5 * DAY]], L, null, now) };
  const b = { weight: 3, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, null, now) };
  assert.ok(Math.abs(ctx.pooledPace([a, b], L, now) - (100 * 5 / 7 + 3 * 100 * 6 / 7) / 4) < 1e-9);
  // Before b's line starts, the total is left empty rather than a's alone.
  assert.equal(ctx.pooledPace([a, b], L, now - 1.5 * DAY), null);
  assert.equal(ctx.pooledPace([], L, now), null);
});

test("normal saves use the backend URL defaults and generate a key when enabling MCP", () => {
  let inputs = [];
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: {}, status: { config: {} },
    $: () => ({ elements: inputs }),
    newMcpKey: () => "llmcp_generated",
  });
  function patch(name, value, type) {
    inputs = [{ name, value, type: type || "text", checked: value, getAttribute: () => null }];
    return JSON.parse(JSON.stringify(ctx.buildPatch(name)));
  }
  assert.deepEqual(patch("cpa_base_url", " http://127.0.0.1:8317/// "), { cpa_base_url: "http://127.0.0.1:8317" });
  assert.deepEqual(patch("cpa_base_url", ""), { cpa_base_url: "http://127.0.0.1:8317" });
  assert.deepEqual(patch("mcp_listen", ""), { mcp_listen: "127.0.0.1:8319" });
  assert.deepEqual(patch("bark_url", "https://api.day.app/test/"), { bark_url: "https://api.day.app/test" });
  ctx.rawConfig.mcp_key = "  ";
  assert.deepEqual(patch("mcp_enabled", true, "checkbox"), { mcp_enabled: true, mcp_key: "llmcp_generated" });
});

test("threshold validation accepts equal levels and rejects reversed levels", () => {
  const elements = {};
  const ctx = context(["thresholdsInOrder"], {
    THRESHOLDS: ["notice_threshold", "low_threshold", "critical_threshold"],
    $: () => ({ elements }),
  });
  function valid(values) {
    ctx.THRESHOLDS.forEach((name, i) => { elements[name] = { value: String(values[i]) }; });
    return ctx.thresholdsInOrder();
  }
  assert.equal(valid([20, 20, 10]), true);
  assert.equal(valid([0, 0, 0]), true);
  assert.equal(valid([10, 20, 10]), false);
});

test("five color levels use raw percentages before display rounding", () => {
  const ctx = context(["quotaThresholds", "quotaLevel"], { LEVEL_EPSILON: 1e-6, status: { config: { notice_threshold: 50, low_threshold: 20 } } });
  assert.equal(ctx.quotaLevel(100), "full");
  assert.equal(ctx.quotaLevel(99.6), "high");
  assert.equal(ctx.quotaLevel(50), "medium");
  assert.equal(ctx.quotaLevel(20), "low");
  assert.equal(ctx.quotaLevel(0), "empty");
});

test("window translations have no duplicate definitions", () => {
  assert.equal((source.match(/window_5h:/g) || []).length, 1);
  assert.equal((source.match(/window_7d:/g) || []).length, 1);
  assert.equal((source.match(/window_30d:/g) || []).length, 1);
  assert.match(source, /window_5h: "5-hour window"/);
});

test("a queued save keeps the value the user chose, whatever the field shows later", () => {
  const input = { name: "mcp_enabled", value: "on", type: "checkbox", checked: false, getAttribute: () => null };
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: { mcp_key: "llmcp_x" }, status: { config: {} },
    $: () => ({ elements: [input] }),
    newMcpKey: () => "llmcp_generated",
  });
  // The user turned MCP off; a late confirmation of an earlier save then
  // refilled the switch as on.
  const intent = { inputs: [input], states: [{ value: "on", checked: false }] };
  input.checked = true;
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled", intent)), { mcp_enabled: false });
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled")), { mcp_enabled: true });
});

test("the confirmation deadline ends a wait even when no status arrives", async () => {
  const ctx = context(["tokenApplied", "whenApplied", "checkApplied"], {
    APPLY_TIMEOUT: 40, setTimeout, clearTimeout, Promise,
    savedTokens: ["a1", "b2"], applyWaits: [], status: { config_applied_token: "old" },
    // The status requests hang: nothing calls checkApplied.
    pollApplied: () => {},
  });
  const started = Date.now();
  assert.equal(await ctx.whenApplied("a1"), false);
  assert.ok(Date.now() - started < 1000);
  assert.equal(ctx.applyWaits.length, 0);
  // The token reported after the deadline settles nothing.
  ctx.status.config_applied_token = "a1";
  ctx.checkApplied();
  assert.equal(ctx.applyWaits.length, 0);

  ctx.status.config_applied_token = "old";
  const pending = ctx.whenApplied("b2");
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.equal(await pending, true);
  assert.equal(ctx.applyWaits.length, 0);
  assert.equal(await ctx.whenApplied("a1"), true);
});

test("a current window whose reset became unknown ends the even pace now", () => {
  const ctx = pace();
  const L = WEEK, now = 5000 * DAY, x = now + 3 * DAY;
  const history = [[now - 2 * DAY, x]];
  // Reset unknown in the status: the line stops now, including the future.
  let segs = ctx.paceSegments(history, L, 0, now);
  assert.ok(ctx.paceAt(segs, L, now - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now), null);
  assert.equal(ctx.paceAt(segs, L, now + HOUR), null);
  assert.equal(ctx.paceAt(segs, L, x - 1), null);
  // Without the window in the status, the history holds.
  segs = ctx.paceSegments(history, L, null, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  segs = ctx.paceSegments(history, L, undefined, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  // Already unknown in the history: nothing more to add.
  assert.deepEqual(plain(ctx.paceSegments([[now - DAY, 0]], L, 0, now)), []);
  // A total breaks with the account whose reset became unknown.
  const a = { weight: 1, segments: ctx.paceSegments(history, L, 0, now) };
  const b = { weight: 1, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, now + 6 * DAY, now) };
  assert.ok(ctx.pooledPace([a, b], L, now - 1) > 0);
  assert.equal(ctx.pooledPace([a, b], L, now), null);
  assert.equal(ctx.pooledPace([a, b], L, now + HOUR), null);
});

// ---- Events ----

// A var statement of page.js, such as the NOTIFY_TRIGGERS table.
function pageVar(name) {
  const match = source.match(new RegExp("\\n  var " + name + " = [^]*?;\\n"));
  assert.ok(match, name);
  return match[0];
}

const EVENT_FUNCTIONS = ["groupLabel", "retryText", "cutText", "errorSummary", "notifyKind", "eventWindow", "sameSubject",
  "eventSeconds", "eventRows", "notifyTitle", "eventView", "eventParts", "eventMasked", "pushMarks", "parseTime"];

// Elements and text nodes as plain objects; translate shows the key and
// its arguments.
function eventContext(globals = {}) {
  const ctx = context(EVENT_FUNCTIONS, {
    status: { accounts: [] }, masked: false, SENSITIVE: "account-sensitive",
    translate: (key, ...args) => args.length ? key + "(" + args.join("|") + ")" : key,
    fmtDateTime: (t) => t ? t.toISOString().slice(5, 16) : "—",
    pct: (v) => v == null || isNaN(v) ? "—" : Math.round(v) + "%",
    el: (tag, attrs = {}, children = []) => ({ tag, ...attrs, children }),
    document: { createTextNode: (text) => ({ text }) },
    maskedNodes: (text, extra) => {
      // Like page.js: each listed name in its own sensitive span.
      let rest = String(text), out = [];
      const names = (extra || []).filter((n) => n && n.trim()).sort((a, b) => b.length - a.length);
      while (rest) {
        let at = -1, hit = "";
        for (const n of names) { const i = rest.indexOf(n); if (i >= 0 && (at < 0 || i < at)) { at = i; hit = n; } }
        if (at < 0) { out.push({ text: rest }); break; }
        if (at) out.push({ text: rest.slice(0, at) });
        out.push({ tag: "span", class: "account-sensitive", text: hit });
        rest = rest.slice(at + hit.length);
      }
      return out;
    },
    ...globals,
  });
  vm.runInContext(["EVENT_TYPES", "NOTIFY_TRIGGERS", "NOTIFY_LAG", "PAUSE_PAIR", "ERROR_CHARS"].map(pageVar).join("\n"), ctx);
  return ctx;
}

const T0 = Date.UTC(2026, 10, 16, 0, 0, 0) / 1000;
function event(kind, at, fields = {}) {
  return { time: new Date((T0 + at) * 1000).toISOString(), kind, level: "info", ...fields };
}

test("a notification joins the row of the event it reports", () => {
  const ctx = eventContext();
  const g = { group: "codex:a", label: "GPT" };
  const events = [
    event("notify", 70, { ...g, params: { title: "✅ GPT · 7d recovered", notify_kind: "recovery", windows: "seven-day" } }),
    event("reset", 60, { ...g, params: { window: "seven-day", kind: "7d", label: "7d", remaining: "100.00" } }),
    event("notify_failed", 40, { ...g, level: "warn", params: { title: "⚠️ GPT ignition paused", notify_kind: "circuit", error: "bark: 500" } }),
    event("notify", 39, { ...g, params: { title: "⚠️ GPT ignition paused", notify_kind: "circuit" } }),
    event("ignite_paused", 30, { ...g, level: "error", params: { until: "2026-11-17T00:00:00Z", error: "boom", summary: "boom" } }),
    // No event before it: a quota alert stays a row of its own.
    event("notify", 20, { ...g, params: { title: "🟡 GPT · 5h 45%", notify_kind: "quota" } }),
    // Too late for the reset of another group.
    event("notify", 10, { group: "codex:b", label: "GPT 2", params: { title: "✅ GPT 2 · 7d recovered", notify_kind: "recovery" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + "@" + (r.event.params.title || ""))), [
    "reset@", "ignite_paused@", "notify@🟡 GPT · 5h 45%", "notify@✅ GPT 2 · 7d recovered"]);
  assert.deepEqual(plain(built.rows[0].pushes.map((ev) => ev.kind)), ["notify"]);
  assert.deepEqual(plain(built.rows[1].pushes.map((ev) => ev.kind).sort()), ["notify", "notify_failed"]);
  const marks = ctx.pushMarks(built.rows[1].pushes);
  assert.deepEqual(plain(marks.map((m) => m.text)), ["ev_notify", "ev_notify_failed"]);
  assert.match(marks[1].title, /bark: 500/);
  assert.equal(marks[1].class, "event-mark failed");
  // A standalone quota alert has its own badge, without the repeated label.
  const view = ctx.eventView(built.rows[2].event, built.rows[2].notifyKind);
  assert.equal(view.badge, "ev_notify_quota");
  assert.equal(view.detail, "🟡 5h 45%");
  // An unmatched recovery falls back to the plain badge.
  assert.equal(ctx.eventView(built.rows[3].event, built.rows[3].notifyKind).badge, "ev_notify");
});

test("matching needs the same group, the order, the time limit and the windows", () => {
  const ctx = eventContext();
  const g = { group: "k", label: "Gemini" };
  const reset = (at, window) => event("reset", at, { ...g, params: { window, remaining: "100.00" } });
  const recovery = (at, windows) => event("notify", at, { ...g, params: { title: "✅ Gemini · 7d recovered", notify_kind: "recovery", windows } });
  // Sent before the event, or more than 70 minutes after it, as retries
  // can be: no match.
  assert.equal(ctx.eventRows([reset(100, "gemini-weekly"), recovery(99, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4301, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4300, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 1);
  // The window must be one the notification names.
  let built = ctx.eventRows([recovery(110, "gemini-weekly"), reset(100, "gemini-5h")], true);
  assert.equal(built.rows.length, 2);
  // A notification for two windows marks both resets.
  built = ctx.eventRows([recovery(110, "gemini-5h,gemini-weekly"), reset(100, "gemini-5h"), reset(100, "gemini-weekly")], true);
  assert.equal(built.rows.length, 2);
  assert.ok(built.rows.every((r) => r.pushes.length === 1));
  // Hidden 5-hour resets take their notifications with them.
  built = ctx.eventRows([recovery(110, "gemini-5h"), reset(100, "gemini-5h"), reset(50, "gemini-weekly")], false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.params.window)), ["gemini-weekly"]);
  assert.equal(built.hidden, 1);
  assert.equal(built.fiveHour, 1);
  // Older notifications have no group: their label decides.
  built = ctx.eventRows([
    event("notify", 20, { label: "Gemini", params: { title: "⚠️ Gemini quota is back, CPA still cooling down" } }),
    event("cooldown_stale", 10, { label: "Gemini", params: { until: "2026-11-17T00:00:00Z" } }),
  ], false);
  assert.equal(built.rows.length, 1);
  assert.equal(built.rows[0].event.kind, "cooldown_stale");
});

test("older notifications are told by their titles", () => {
  const ctx = eventContext();
  const kind = (title, kindName = "notify") => ctx.notifyKind({ kind: kindName, params: { title } });
  assert.equal(kind("✅ GPT · 7d recovered"), "recovery");
  assert.equal(kind("⚠️ Gemini ignition paused"), "circuit");
  assert.equal(kind("🎫 GPT · plan plus → free"), "plan");
  assert.equal(kind("⚠️ GPT quota is back, CPA still cooling down"), "cooldown");
  assert.equal(kind("🟡 GPT · 5h 45%"), "quota");
  assert.equal(kind("🔴 GPT · 5h 5%"), "quota");
  assert.equal(kind("⏰ GPT · 5h reset reminder"), "reminder");
  // Codex reset signals also start with ✅ but report no reset of an account.
  assert.equal(kind("✅ Codex global reset completed"), "codex_reset");
  assert.equal(kind("🕯️ cpa-pulse test notification"), "");
  // Failed notifications recorded without params.
  assert.equal(ctx.notifyKind({ kind: "notify_failed", detail: "✅ GPT · 7d recovered: timeout" }), "recovery");
  assert.equal(ctx.notifyKind({ kind: "notify", params: { title: "x", notify_kind: "suspended" } }), "suspended");
});

test("resets and exhaustion name the window by its short label", () => {
  const ctx = eventContext();
  assert.deepEqual(plain(ctx.eventWindow({ window: "five-hour" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "gemini-5h" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "3p-weekly" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus" })), { kind: "", label: "seven-day-opus" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus", kind: "", label: "Opus 7d" })), { kind: "", label: "Opus 7d" });
  let view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00", reset: "2026-11-23T00:00:00Z" } }));
  assert.equal(view.badge, "ev_reset");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_window_next(7d|100%|11-23T00:00)");
  view = ctx.eventView(event("exhausted", 0, { label: "GPT", params: { window: "five-hour", kind: "5h", label: "5h", remaining: "0.00", reset: "2026-11-16T05:00:00Z" } }));
  assert.equal(view.badge, "ev_exhausted");
  assert.equal(view.detail, "ev_window_due(5h|0%|11-16T05:00)");
  view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00" } }));
  assert.equal(view.detail, "ev_window_left(7d|100%)");
  // Exhaustion of a 5-hour window always shows.
  const built = ctx.eventRows([event("exhausted", 0, { params: { window: "five-hour", remaining: "0" } })], false);
  assert.equal(built.rows.length, 1);
});

test("error texts shrink to one line and stay masked", () => {
  const ctx = eventContext();
  const google = 'host_call_failed: {\n  "error": {\n    "code": 403,\n    "message": "The caller does not have permission for alice@example.com",\n' +
    '    "status": "PERMISSION_DENIED",\n    "details": [{ "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "TOS_VIOLATION" }]\n  }\n} (HTTP 403)';
  assert.equal(ctx.errorSummary(google), "host_call_failed: HTTP 403 · TOS_VIOLATION · The caller does not have permission for alice@example.com");
  assert.equal(ctx.errorSummary('{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}} (HTTP 429)'),
    "HTTP 429 · rate_limit_error · Rate limit reached");
  assert.equal(ctx.errorSummary('Refresh failed: {"error":"invalid_grant","error_description":"Token has been expired"}'), "Refresh failed: invalid_grant · Token has been expired");
  assert.equal(ctx.errorSummary("dial tcp: timeout\nmore lines"), "dial tcp: timeout");
  assert.equal(ctx.errorSummary("{not json\nsecond"), "{not json");
  const long = ctx.errorSummary("x".repeat(400));
  assert.equal(Array.from(long).length, 160);
  assert.ok(long.endsWith("…"));

  const ev = event("ignite_failed", 0, { group: "g", label: "alice", params: { retry_seconds: "300", error: google } });
  const view = ctx.eventView(ev);
  assert.ok(!view.detail.includes("\n"));
  assert.ok(view.detail.includes("TOS_VIOLATION"));
  assert.ok(view.tip.includes('"code": 403'));
  // The summary from the backend wins over the page's own.
  assert.ok(ctx.eventView({ ...ev, params: { ...ev.params, summary: "Account banned" } }).detail.includes("Account banned"));
  // Masked, neither the label nor the error leaves a plain text node.
  const nodes = ctx.eventMasked(ev, view);
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  assert.equal(plainText(nodes.label), "");
  assert.ok(!plainText(nodes.detail).includes("alice"), plainText(nodes.detail));
  assert.ok(!plainText(nodes.detail).includes("TOS_VIOLATION"));
  // The tooltips of notification marks are left out while masked.
  ctx.masked = true;
  const marks = ctx.pushMarks([event("notify_failed", 0, { params: { title: "⚠️ alice ignition paused", error: "x" } })]);
  assert.equal(marks[0].title, null);
});

test("credential events show the reason, a summary and an appeal link", () => {
  const ctx = eventContext();
  const invalid = (params) => ctx.eventView(event("credential_invalid", 0, { level: "error", label: "Gemini", params }));
  let view = invalid({ reason: "suspended", status: "403", code: "TOS_VIOLATION", summary: "Account suspended for violating terms of service", appeal_url: "https://support.example.com/appeal", source: "ignite" });
  assert.equal(view.badge, "ev_credential_suspended");
  assert.equal(view.style, "failure");
  // Found by ignition: the row says ignition is paused.
  assert.equal(view.detail, "ev_ignite_paused_note(Account suspended for violating terms of service)");
  assert.equal(view.link, "https://support.example.com/appeal");
  // The tooltip shows the status, code and summary, and the appeal address.
  assert.equal(view.tip, "HTTP 403 · TOS_VIOLATION · Account suspended for violating terms of service\nhttps://support.example.com/appeal");
  assert.ok(view.secrets.includes("Account suspended for violating terms of service"));
  view = invalid({ reason: "auth", summary: "", status: "401", appeal_url: "javascript:alert(1)" });
  assert.equal(view.badge, "ev_credential_invalid");
  assert.equal(view.detail, "HTTP 401");
  assert.equal(view.link, "");
  view = invalid({ reason: "rate_limited", summary: "Too many requests", until: "2026-11-16T08:00:00Z" });
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_limited_until(Too many requests|11-16T08:00)");
  view = ctx.eventView(event("credential_restored", 0, { label: "Gemini", params: { source: "query" } }));
  assert.equal(view.badge, "ev_credential_restored");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_source_query");
  // A credential notification joins its event.
  const built = ctx.eventRows([
    event("notify", 5, { group: "k", label: "Gemini", params: { title: "⚠️ Gemini", notify_kind: "suspended" } }),
    event("credential_invalid", 0, { group: "k", label: "Gemini", params: { reason: "suspended" } }),
    event("notify", 5, { group: "j", label: "Claude", params: { title: "✅ Claude", notify_kind: "credential_ok" } }),
    event("credential_restored", 0, { group: "j", label: "Claude", params: { source: "query" } }),
  ], false);
  assert.deepEqual(plain(built.rows.map((r) => [r.event.kind, r.pushes.length])), [["credential_invalid", 1], ["credential_restored", 1]]);
});

// ---- Even pace extended back to the cycle start ----

function extension() {
  return context(["paceSegments", "paceAt", "pooledPace", "paceExtensions", "extendedPace"], { RESET_DRIFT: 60, RESET_JUMP: 5 });
}

// Synthetic samples shaped like a 7-day window whose quota came back to
// 100% at rise and whose scheduled reset moved back to an earlier time at
// move: [time, remaining].
function movedReset() {
  const rise = 6000 * DAY, x1 = rise + WEEK + 4 * 60 + 14, x2 = rise + 6 * DAY - 52 * 60, move = rise + 16.5 * HOUR;
  const points = [[rise - 2 * HOUR, 22], [rise - HOUR, 21], [rise, 100], [rise + 6 * HOUR, 91], [move - 2 * HOUR, 65], [move, 21], [move + HOUR, 20]];
  // The history records a change of the reset only beyond 60 s; the
  // reset wavered between x1 and x1 + 1 meanwhile.
  const resets = [[rise - 3 * DAY, x2], [rise, rise + WEEK], [rise + 4 * 60, x1], [move, x2]];
  return { rise, x1, x2, move, points, resets };
}

test("a reset that moves within its cycle extends the new line back to the cycle start", () => {
  const ctx = extension();
  const d = movedReset();
  const segs = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + 2 * HOUR);
  const ext = ctx.paceExtensions(segs, d.points, WEEK);
  assert.deepEqual(plain(ext), [[d.rise, d.move, d.x2]]);
  // The old line stays: just before the move it follows the reset x1.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, d.move - 1) - 100 * (d.x1 - d.move + 1) / WEEK) < 1e-6);
  // The extension runs the line of x2 back to the rise.
  const part = { weight: 1, segments: segs, extensions: ext };
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.rise) - 100 * (d.x2 - d.rise) / WEEK) < 1e-9);
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.move) - ctx.paceAt(segs, WEEK, d.move)) < 1e-9);
  // Outside the extension there is none.
  assert.equal(ctx.extendedPace([part], WEEK, d.rise - 1), null);
  assert.equal(ctx.extendedPace([part], WEEK, d.move + 1), null);
});

test("resets that waver by a second extend nothing", () => {
  const ctx = extension();
  const d = movedReset();
  // The same cycle recorded with every one-second change, as a backend
  // without the 60 s tolerance would: x1, x1 + 1, x1, x1 + 1.
  const resets = [[d.rise, d.rise + WEEK], [d.rise + 4 * 60, d.x1], [d.rise + 6 * HOUR, d.x1 + 1], [d.rise + 8 * HOUR, d.x1], [d.rise + 9 * HOUR, d.x1 + 1]];
  const segs = ctx.paceSegments(resets, WEEK, d.x1, d.rise + 10 * HOUR);
  assert.deepEqual(plain(ctx.paceExtensions(segs, d.points, WEEK)), []);
  // The status reset that wavers adds no segment either.
  assert.equal(ctx.paceSegments(resets, WEEK, d.x1 + 1, d.rise + 10 * HOUR).length, segs.length);
  // A line that starts with its cycle, or an early reset, extends nothing.
  const early = d.rise + 2 * DAY;
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.rise, d.x1], [early, early + WEEK]], WEEK, null, early + HOUR),
    [[d.rise, 100], [early - HOUR, 40], [early, 100]], WEEK)), []);
  // Without a rise in the samples, the start of the cycle is not known.
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.move, d.x2]], WEEK, null, d.move + HOUR), [[d.move, 21]], WEEK)), []);
});

test("a total extends where an account does, with the other accounts' lines", () => {
  const ctx = extension();
  const d = movedReset();
  const segsA = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + HOUR);
  const a = { weight: 1, segments: segsA, extensions: ctx.paceExtensions(segsA, d.points, WEEK) };
  const xb = d.rise + 3 * DAY;
  const segsB = ctx.paceSegments([[xb - WEEK, xb]], WEEK, xb, d.move + HOUR);
  const b = { weight: 3, segments: segsB, extensions: ctx.paceExtensions(segsB, [], WEEK) };
  const t = d.rise + HOUR;
  const want = (100 * (d.x2 - t) / WEEK + 3 * 100 * (xb - t) / WEEK) / 4;
  assert.ok(Math.abs(ctx.extendedPace([a, b], WEEK, t) - want) < 1e-9);
  assert.equal(ctx.extendedPace([b], WEEK, t), null);
});

test("the detail heading moves left of the plot only where it fits", () => {
  const ctx = context(["placeDetailHead"], { SIDE_GAP: 46 });
  const wrap = () => {
    const classes = new Set(), style = {};
    return { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) }, style: { setProperty: (k, v) => { style[k] = v; } }, classes, css: style };
  };
  const box = { clientWidth: 1000, offsetHeight: 206 };
  let w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 180 }, box, { labelW: 190, width: 1000 });
  assert.ok(w.classes.has("side"));
  assert.equal(w.css["--detail-side"], "144px");
  // Too narrow: the blank of a chart below 640 pixels.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 100 }, { clientWidth: 600, offsetHeight: 206 }, { labelW: 120, width: 600 });
  assert.ok(!w.classes.has("side"));
  // Too tall, such as a long name over several lines.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 240 }, box, { labelW: 190, width: 1000 });
  assert.ok(!w.classes.has("side"));
});

test("manual ignition failures show no retry, and credential pauses no end time", () => {
  const ctx = eventContext();
  const json = '{"error":{"code":401,"message":"token expired","status":"UNAUTHENTICATED"}}';
  let view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json, summary: "Credential expired" } }));
  assert.equal(view.detail, "Credential expired");
  assert.equal(view.tip, json);
  view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json } }));
  assert.equal(view.detail, "HTTP 401 · UNAUTHENTICATED · token expired");
  assert.ok(view.secrets.includes(json));
  // Automatic failures keep the retry; older manual failures without params
  // keep their stored text.
  assert.match(ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { retry_seconds: "300", error: "x" } })).detail, /^ev_retry\(/);
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "GPT", detail: "Ignition failed: x" })).detail, "Ignition failed: x");

  const paused = (params) => ctx.eventView(event("ignite_paused", 0, { level: "error", label: "Gemini", params }));
  assert.equal(paused({ reason: "auth", error: json, summary: "Credential expired" }).detail, "ev_paused_auth(Credential expired)");
  view = paused({ reason: "suspended", error: json });
  assert.equal(view.detail, "ev_paused_suspended(HTTP 401 · UNAUTHENTICATED · token expired)");
  assert.equal(view.tip, "ev_paused_suspended(" + json + ")");
  assert.equal(paused({ reason: "rate_limited", until: "2026-11-17T00:00:00Z", error: "x", summary: "Too many requests" }).detail, "ev_paused(11-17T00:00|Too many requests)");
  assert.equal(paused({ until: "2026-11-17T00:00:00Z", error: "boom" }).detail, "ev_paused(11-17T00:00|boom)");
  // Without an end time or a known reason, no "until —".
  assert.equal(paused({ error: "boom" }).detail, "boom");
});

test("a credential problem shares its row with the ignition pause and the notifications", () => {
  const ctx = eventContext();
  const g = { group: "claude:k", label: "Claude" };
  const until = "2026-11-16T23:00:00Z";
  const events = [
    event("notify", 1800, { ...g, params: { title: "⏳ Claude rate limited", notify_kind: "rate_limited" } }),
    event("notify_failed", 30, { ...g, level: "warn", params: { title: "⚠️ Claude ignition paused", notify_kind: "circuit", error: "Bark: 500" } }),
    event("credential_invalid", 0, { ...g, level: "error", params: { reason: "rate_limited", status: "429", summary: "Too many requests", until, source: "ignite" } }),
    event("ignite_paused", 0, { ...g, level: "error", params: { until, error: '{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}', summary: "Too many requests" } }),
    // Another group's pause stays its own row.
    event("ignite_paused", 10, { group: "gemini:k", label: "Gemini", level: "error", params: { until, error: "x" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + ":" + r.event.label)), ["credential_invalid:Claude", "ignite_paused:Gemini"]);
  const row = built.rows[0];
  assert.equal(row.paused.kind, "ignite_paused");
  // The rate limit notification half an hour later and the circuit
  // notification of the pause both join it.
  assert.deepEqual(plain(row.pushes.map((ev) => ev.params.notify_kind).sort()), ["circuit", "rate_limited"]);
  const view = ctx.eventView(row.event, row.notifyKind, row.paused);
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_ignite_paused_until(Too many requests|11-16T23:00)");
  // The tooltip adds the full error of the pause on a line of its own.
  assert.equal(view.tip, 'HTTP 429 · Too many requests\n{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}');
  // A pause more than PAUSE_PAIR seconds away is a row of its own.
  assert.equal(ctx.eventRows([events[2], { ...events[3], time: new Date((T0 - 61) * 1000).toISOString() }], false).rows.length, 2);
  // Found by a query, without a pause: the rate limit alone.
  assert.equal(ctx.eventView(event("credential_invalid", 0, { ...g, params: { reason: "rate_limited", summary: "Too many requests", until, source: "query" } })).detail,
    "ev_limited_until(Too many requests|11-16T23:00)");
  // An account suspended at ignition: one row too.
  const suspended = ctx.eventRows([
    event("credential_invalid", 0, { ...g, params: { reason: "suspended", summary: "Suspended", source: "ignite" } }),
    event("ignite_paused", 0, { ...g, params: { reason: "suspended", error: "x", summary: "Suspended" } }),
  ], false);
  assert.equal(suspended.rows.length, 1);
  const merged = ctx.eventView(suspended.rows[0].event, undefined, suspended.rows[0].paused);
  assert.equal(merged.detail, "ev_ignite_paused_note(Suspended)");
  // With an appeal address, the pause's error follows it; a pause without
  // an error adds nothing.
  const appeal = { reason: "suspended", status: "403", summary: "Suspended", appeal_url: "https://appeal.example.com/", source: "ignite" };
  const withPause = (error) => ctx.eventView(event("credential_invalid", 0, { ...g, params: appeal }), undefined,
    event("ignite_paused", 0, { ...g, params: error ? { reason: "suspended", error, summary: "Suspended" } : { reason: "suspended", summary: "Suspended" } }));
  assert.equal(withPause("raw").tip, "HTTP 403 · Suspended\nhttps://appeal.example.com/\nraw");
  assert.equal(withPause("").tip, "HTTP 403 · Suspended\nhttps://appeal.example.com/");
});

test("Did Codex Reset notifications join the event recorded after them", () => {
  const ctx = eventContext();
  const signal = (at) => event("codex_reset", at, { label: "Did Codex Reset", params: { count: "1" } });
  const push = (at, fields = {}) => event("notify", at, { params: { title: "✅ Codex global reset completed", notify_kind: "codex_reset" }, ...fields });
  let built = ctx.eventRows([signal(10), push(5), push(0, { params: { title: "📅 Codex global reset scheduled" } })], false);
  assert.equal(built.rows.length, 1);
  assert.equal(built.rows[0].event.kind, "codex_reset");
  assert.equal(built.rows[0].pushes.length, 2);
  // More than five minutes apart: the notification stays a row, under the
  // label of Did Codex Reset.
  built = ctx.eventRows([signal(400), push(0)], false);
  assert.equal(built.rows.length, 2);
  const view = ctx.eventView(built.rows[1].event, built.rows[1].notifyKind);
  assert.equal(view.label, "Did Codex Reset");
  assert.equal(view.detail, "✅ Codex global reset completed");
});

test("older failure events without params hide their whole detail when masked", () => {
  const ctx = eventContext();
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  // A manual ignition failure from before params, naming an account that
  // has since been removed: no name list can catch it.
  const old = [
    event("ignite_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "Manual ignition failed: permission denied for gone@example.com" }),
    event("ignite_paused", 0, { group: "claude:k", label: "Claude", level: "error", detail: "Paused until 11/17: gone@example.com" }),
    event("notify_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "Bark: gone@example.com" }),
    event("error", 0, { level: "error", label: "Claude", detail: "Quota query failed: gone@example.com" }),
  ];
  for (const ev of old) {
    const view = ctx.eventView(ev);
    assert.ok(view.whole, ev.kind);
    const nodes = ctx.eventMasked(ev, view);
    assert.equal(plainText(nodes.detail), "", ev.kind);
    assert.ok(nodes.detail.some((n) => n.text.includes("gone@example.com")), ev.kind);
  }
  // An older success keeps its detail, with only the names hidden.
  const ok = event("ignite", 0, { group: "claude:k", label: "Claude", detail: "claude-haiku · next reset 11/17" });
  assert.equal(ctx.eventView(ok).whole, false);
  assert.equal(plainText(ctx.eventMasked(ok).detail), "claude-haiku · next reset 11/17");
  // An older success without a stored label hides whole too, even when its
  // group gives a label now: the account may have been renamed since.
  const ctx2 = eventContext({ status: { accounts: [{ groups: [{ key: "claude:k", label: "Claude Now" }] }] } });
  const unlabeled = event("ignite", 0, { group: "claude:k", message: "Claude Old Ignition succeeded (claude-haiku), next reset 11/17" });
  const view = ctx2.eventView(unlabeled);
  assert.equal(view.label, "Claude Now");
  assert.ok(view.whole);
  const nodes = ctx2.eventMasked(unlabeled, view);
  assert.equal(plainText(nodes.detail), "");
  assert.ok(nodes.detail.some((n) => n.text.includes("Claude Old")));
  // Events with params keep the masking by names and error texts.
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "Claude", params: { manual: "1", error: "x", summary: "y" } })).whole, false);
});

function pooledContext() {
  return context(["valueAt", "pooled", "resetAsOf", "gapValue", "estimatedRuns"], { RESET_JUMP: 5 });
}

test("a pooled total estimates an account missing data rather than leaving it out", () => {
  const ctx = pooledContext();
  const now = Math.floor(Date.now() / 1000), H = 3600, limit = 900;
  const t0 = now - 4 * H;
  const every = (from, to, value) => { const out = []; for (let t = from; t <= to; t += 300) out.push([t, value, 0]); return out; };
  // a stays full; b reads 51% until a two-hour gap, then a passive 52%
  // (a header reads a point above the usage API) and 51% again.
  const a = { weight: 1, resets: [[t0, now + 5 * 86400]], points: every(t0, now, 100) };
  const b = { weight: 1, resets: [[t0, now + 2 * 86400]],
    points: every(t0, t0 + H, 51).concat([[t0 + 3 * H, 52, 1]], every(t0 + 3 * H + 300, now, 51)) };
  a.floor = a.points; b.floor = b.points;
  const out = ctx.pooled([a, b], limit, "points");
  const gap = out.filter((p) => p[0] > t0 + H + limit && p[0] < t0 + 3 * H);
  assert.ok(gap.length > 10);
  // Never the 100% of a alone; b holds 51% and the total stays at 75.5%.
  gap.forEach((p) => { assert.equal(p[1], 75.5); assert.equal(p[5], 1); });
  const measured = out.filter((p) => p[0] <= t0 + H + limit || p[0] >= t0 + 3 * H);
  measured.forEach((p) => assert.equal(p[5], 0));
  // With no account fresh there is no point: a gap of the total.
  const both = ctx.pooled([{ weight: 1, resets: [], points: [[t0, 50, 0], [t0 + 2 * H, 40, 0]] },
    { weight: 1, resets: [], points: [[t0, 70, 0], [t0 + 2 * H, 60, 0]] }], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(both.map((p) => p[0]))), [t0, t0 + 2 * H]);

  // A reset within the gap shows at the next sample of a fresh account;
  // the total gets no point of its own there, so a stretch without any
  // fresh account stays a gap.
  const resetB = { weight: 1, resets: [[t0, t0 + H + 1800]], points: [[t0, 20, 0], [t0 + 3 * H, 100, 0]] };
  const fresh = { weight: 1, resets: [], points: every(t0, t0 + 3 * H, 50) };
  const total = ctx.pooled([resetB, fresh], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(total.filter((p) => p[0] === t0 + H + 1800).map((p) => p[1]))), [75]);
  assert.deepEqual(JSON.parse(JSON.stringify(total.filter((p) => p[0] === t0 + H + 1500).map((p) => p[1]))), [35]);
  const lone = ctx.pooled([resetB, { weight: 1, resets: [], points: [[t0, 50, 0], [t0 + 3 * H, 50, 0]] }], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(lone.map((p) => p[0]))), [t0, t0 + 3 * H]);
});

test("an estimate falls between the samples around a gap and returns to full at a reset", () => {
  const ctx = pooledContext();
  const limit = 900, t0 = 1000000;
  const plain = (v) => JSON.parse(JSON.stringify(v));
  // The same cycle: straight from one sample to the next, never above the first.
  const falling = [[t0, 60, 0], [t0 + 4000, 40, 0]];
  assert.deepEqual(plain(ctx.gapValue(falling, [[t0, t0 + 9000]], t0 + 2000, limit)), { value: 50, estimated: true });
  const rising = [[t0, 51, 0], [t0 + 4000, 53, 1]];
  assert.equal(ctx.gapValue(rising, [[t0, t0 + 9000]], t0 + 2000, limit).value, 51);
  // A recent sample is measured.
  assert.deepEqual(plain(ctx.gapValue(falling, [], t0 + 600, limit)), { value: 60, estimated: false });
  assert.equal(ctx.gapValue(falling, [], t0 - 1, limit), null);
  // A reset within the gap: the last value until the reset, then from 100%
  // to the next sample.
  const across = [[t0, 10, 0], [t0 + 4000, 80, 0]];
  const resets = [[t0, t0 + 2000], [t0 + 4000, t0 + 20000]];
  assert.equal(ctx.gapValue(across, resets, t0 + 1999, limit).value, 10);
  assert.equal(ctx.gapValue(across, resets, t0 + 2000, limit).value, 100);
  assert.equal(ctx.gapValue(across, resets, t0 + 3000, limit).value, 90);
  // A rise by a reset of unknown time holds the last value.
  assert.equal(ctx.gapValue(across, [], t0 + 3000, limit).value, 10);
  // After the last sample: the last value until the reset, then 100%.
  assert.equal(ctx.gapValue([[t0, 30, 0]], [[t0, t0 + 2000]], t0 + 1500, limit).value, 30);
  assert.equal(ctx.gapValue([[t0, 30, 0]], [[t0, t0 + 2000]], t0 + 2500, limit).value, 100);
});

test("estimated stretches of a line are split off and share their ends", () => {
  const ctx = pooledContext();
  const plain = (v) => JSON.parse(JSON.stringify(v));
  const pts = [[1, 80, 0, 0, 0, 0], [2, 79, 0, 0, 0, 1], [3, 78, 0, 0, 0, 1], [4, 77, 0, 0, 0, 0], [5, 76, 0, 0, 0, 0]];
  assert.deepEqual(plain(ctx.estimatedRuns(pts).map((r) => [r.estimated, r.pts.map((p) => p[0])])),
    [[true, [1, 2, 3, 4]], [false, [4, 5]]]);
  assert.deepEqual(plain(ctx.estimatedRuns([[1, 50, 0]]).map((r) => [r.estimated, r.pts.length])), [[false, 1]]);
});

// A localStorage with only the calls the page makes.
function fakeStorage(items = {}) {
  return {
    items,
    getItem: (k) => (Object.prototype.hasOwnProperty.call(items, k) ? items[k] : null),
    setItem: (k, v) => { items[k] = String(v); },
    removeItem: (k) => { delete items[k]; },
  };
}

test("expanded groups are kept in this browser while their switch is on", () => {
  const STORE = "cpa-pulse-chart-expanded", FILTERS = "cpa-pulse-chart-filters";
  const elements = { remember_chart_expanded: { name: "remember_chart_expanded", checked: true }, remember_chart_filters: { name: "remember_chart_filters", checked: false } };
  const storage = fakeStorage({ [STORE]: JSON.stringify(["claude|Claude", "codex|gone", 3, ""]), [FILTERS]: "{}" });
  const ctx = context(["rememberChart", "rememberExpanded", "expandedGroups", "storedExpanded", "writeExpanded", "saveGroupExpanded",
    "saveChartExpanded", "restoreChartExpanded", "changeChartSwitch"], {
    $: () => ({ elements }), localStorage: storage, chartExpanded: {}, JSON,
    CHART_STORE: FILTERS, CHART_EXPANDED_STORE: STORE, saveChartFilters: () => {},
  });
  const saved = () => (STORE in storage.items ? JSON.parse(storage.items[STORE]) : null);
  // Restoring expands the saved groups; names that are not strings are dropped.
  ctx.restoreChartExpanded();
  assert.deepEqual(Object.keys(ctx.chartExpanded), ["group:claude|Claude", "group:codex|gone"]);
  // Expanding or collapsing a group changes only its name in the saved
  // list, read again first: what another tab saved meanwhile stays, a group
  // without a row stays, and a group this tab still shows expanded but
  // another tab collapsed is not written back.
  storage.items[STORE] = JSON.stringify(["claude|Claude", "kimi|Kimi"]);
  ctx.chartExpanded["group:xai|Grok"] = true;
  ctx.saveGroupExpanded("group:xai|Grok");
  assert.deepEqual(saved(), ["claude|Claude", "kimi|Kimi", "xai|Grok"]);
  ctx.chartExpanded["group:claude|Claude"] = false;
  ctx.saveGroupExpanded("group:claude|Claude");
  assert.deepEqual(saved(), ["kimi|Kimi", "xai|Grok"]);
  ctx.saveGroupExpanded("codex|one");
  assert.deepEqual(saved(), ["kimi|Kimi", "xai|Grok"], "only totals are kept");
  // Collapsing the last group deletes the record.
  storage.items[STORE] = JSON.stringify(["xai|Grok"]);
  ctx.chartExpanded["group:xai|Grok"] = false;
  ctx.saveGroupExpanded("group:xai|Grok");
  assert.equal(saved(), null);
  // A damaged record is ignored.
  ctx.chartExpanded = {};
  for (const bad of ["{not json", JSON.stringify({ "claude|Claude": true })]) {
    storage.items[STORE] = bad;
    ctx.restoreChartExpanded();
    assert.deepEqual(ctx.chartExpanded, {});
  }

  // Turning the switch off deletes the record and stops saving; turning it
  // on adds the groups expanded now to what is saved.
  storage.items[STORE] = JSON.stringify(["claude|Claude"]);
  elements.remember_chart_expanded.checked = false;
  ctx.changeChartSwitch(elements.remember_chart_expanded);
  assert.equal(saved(), null);
  assert.equal(storage.items[FILTERS], "{}", "the other switch keeps its record");
  ctx.chartExpanded = { "group:claude|Claude": true };
  ctx.saveGroupExpanded("group:claude|Claude");
  ctx.saveChartExpanded();
  assert.equal(saved(), null);
  storage.items[STORE] = JSON.stringify(["kimi|Kimi"]);
  elements.remember_chart_expanded.checked = true;
  ctx.changeChartSwitch(elements.remember_chart_expanded);
  assert.deepEqual(saved(), ["kimi|Kimi", "claude|Claude"]);
  elements.remember_chart_filters.checked = false;
  ctx.changeChartSwitch(elements.remember_chart_filters);
  assert.equal(FILTERS in storage.items, false);
  assert.notEqual(saved(), null);
  // A page that opens with the switch off, turned off in config.yaml or in
  // another browser, deletes the record and expands nothing.
  elements.remember_chart_expanded.checked = false;
  ctx.chartExpanded = {};
  ctx.restoreChartExpanded();
  assert.equal(saved(), null);
  assert.deepEqual(ctx.chartExpanded, {});

  // Totals use "group:" and the provider and label as their row id, a click
  // that expands or collapses one saves it, and opening the page restores
  // both records or deletes those whose switch is off.
  assert.match(source, /id: "group:" \+ id,/);
  assert.match(source, /chartExpanded\[r\.id\] = !chartExpanded\[r\.id\];\s*saveGroupExpanded\(r\.id\);/);
  const restore = context(["restoreChartFilters"], {
    restoreChartExpanded: () => {}, rememberChart: () => false, localStorage: fakeStorage({ [FILTERS]: "{}" }), CHART_STORE: FILTERS,
  });
  restore.restoreChartFilters();
  assert.equal(FILTERS in restore.localStorage.items, false);
  assert.match(source, /function restoreChartFilters\(\) \{\s*restoreChartExpanded\(\);/);
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /name="remember_chart_filters"[^]*name="remember_chart_expanded"/);
});

test("the public MCP address accepts only http and https addresses", () => {
  const url = context(["publicUrl"]);
  for (const name of ["PUBLIC_URL_TRIM", "PUBLIC_URL_FORM"]) vm.runInContext(pageVar(name), url);
  const cases = JSON.parse(fs.readFileSync(path.join(__dirname, "testdata", "public_urls.json"), "utf8"));
  assert.ok(cases.length > 30);
  for (const [value, want] of cases) assert.equal(url.publicUrl(value), want, JSON.stringify(value));

  const STORE = "cpa-pulse-mcp-url";
  const storage = fakeStorage();
  const field = { value: "" };
  let renders = 0;
  const base = context(["publicUrl", "storedPublicUrl", "savePublicUrl", "syncPublicUrl", "mcpBaseUrl"], {
    $: (id) => (id === "mcp-public-url" ? field : { elements: { mcp_listen: { value: "0.0.0.0:8319" } } }),
    status: null, MCP_LISTEN: "127.0.0.1:8319", MCP_URL_STORE: STORE, location: { hostname: "nas" },
    localStorage: storage, renderMcp: () => { renders++; },
  });
  for (const name of ["PUBLIC_URL_TRIM", "PUBLIC_URL_FORM"]) vm.runInContext(pageVar(name), base);
  base.savePublicUrl("mcp.example.com/mcp/");
  assert.equal(base.mcpBaseUrl(), "https://mcp.example.com");
  base.savePublicUrl("  ");
  assert.equal(STORE in storage.items, false, "an empty address is deleted");
  assert.equal(base.mcpBaseUrl(), "http://nas:8319");
  base.savePublicUrl("ftp://x.com");
  assert.equal(base.mcpBaseUrl(), "http://nas:8319", "an invalid address falls back to the listen address");
  // /mcp comes off the path only, never off a host named mcp.
  for (const [value, want] of [["mcp", "https://mcp"], ["mcp/", "https://mcp"], ["mcp/mcp", "https://mcp"], ["http://mcp:8319/mcp/", "http://mcp:8319"],
    ["https://x.com/a/mcp//", "https://x.com/a"], ["https://x.com/mcpx", "https://x.com/mcpx"], ["https://x.com/a/", "https://x.com/a"]]) {
    base.savePublicUrl(value);
    assert.equal(base.mcpBaseUrl(), want, value);
  }
  // Saving trims what the check trims, so an address keeps its verdict
  // when the page opens again: U+FEFF is not white space here.
  for (const [value, want] of cases) {
    base.savePublicUrl(value);
    assert.equal(base.publicUrl(base.storedPublicUrl()), want, JSON.stringify(value));
  }
  base.savePublicUrl("\u3000https://x.com\u0085");
  assert.equal(storage.items[STORE], "https://x.com");

  // The configs follow the saved address, not this tab's field, and a change
  // made in another tab updates the field and the configs.
  base.savePublicUrl("https://new.example.com");
  field.value = "https://old.example.com";
  assert.equal(base.mcpBaseUrl(), "https://new.example.com");
  base.syncPublicUrl("cpa-pulse-view");
  assert.equal(field.value, "https://old.example.com");
  assert.equal(renders, 0);
  base.syncPublicUrl(STORE);
  assert.equal(field.value, "https://new.example.com");
  delete storage.items[STORE];
  base.syncPublicUrl(null);
  assert.equal(field.value, "");
  assert.equal(base.mcpBaseUrl(), "http://nas:8319");
  assert.equal(renders, 2);
  assert.match(source.match(/window\.addEventListener\("storage", function \(e\) \{[^]*?\n  \}\);/)[0], /syncPublicUrl\(e\.key\);/);
  // Without localStorage the field is used.
  base.localStorage = { getItem: () => { throw new Error("denied"); } };
  field.value = " mcp.example.com ";
  assert.equal(base.mcpBaseUrl(), "https://mcp.example.com");

  // An invalid address is pointed out next to the field.
  assert.match(source, /\$\("mcp-public-url-error"\)\.hidden = !invalid;/);
  assert.match(fs.readFileSync(path.join(__dirname, "page.html"), "utf8"), /id="mcp-public-url-error"[^>]*data-i18n="mcp_public_url_invalid"[^>]*hidden/);
});

test("the client commands quote the address and the key for the shell, and the JSON is built by JSON.stringify", () => {
  const { execFileSync } = require("node:child_process");
  const ctx = context(["mcpSnippets", "shellQuote"]);
  const url = "https://x.com/'$(touch /tmp/pwned)'`id`/mcp", secret = "k'ey \"$HOME\"";
  const snippets = Object.fromEntries(ctx.mcpSnippets(url, secret));
  assert.equal(ctx.shellQuote("a'b"), "'a'\\''b'");
  // The shell reads each command back as the words it was built from.
  const words = (command) => execFileSync("sh", ["-c", "for w in " + command.replace(/^\S+ /, "") + "; do printf '%s\\n' \"$w\"; done"], { encoding: "utf8" }).trim().split("\n");
  assert.deepEqual(words(snippets.mcp_claude), ["mcp", "add", "--transport", "http", "cpa-pulse", url, "--header", "Authorization: Bearer " + secret]);
  assert.deepEqual(words(snippets.mcp_stdio), ["mcp-remote", url, "--header", "Authorization: Bearer " + secret]);
  assert.deepEqual(JSON.parse(snippets.mcp_json), { mcpServers: { "cpa-pulse": { type: "http", url, headers: { Authorization: "Bearer " + secret } } } });
});

function priceNode(tag, attrs = {}, children = []) {
  attrs = attrs || {};
  const node = saveNode(tag.toUpperCase(), attrs);
  node.className = attrs.class || "";
  node.disabled = !!attrs.disabled;
  node.hidden = !!attrs.hidden;
  node.value = attrs.value ?? "";
  node.events = {};
  for (const [key, value] of Object.entries(attrs)) {
    if (key.startsWith("on")) node.events[key.slice(2)] = value;
  }
  if (attrs.text !== undefined) node.textContent = String(attrs.text);
  node.children.push(...children);
  node.focus = () => { node.focused = true; };
  node.blur = () => {};
  node.reportValidity = () => true;
  return node;
}

function descendants(node, predicate) {
  return [node, ...node.children.flatMap(child => descendants(child, predicate))].filter(predicate);
}

function pricingContext(extraNames = [], globals = {}) {
  const nodes = {};
  const $ = id => nodes[id] || (nodes[id] = priceNode("div"));
  $("prices-scope").value = "cpa";
  const entries = [
    { provider: "anthropic", model: "haiku", has_price: true, input_usd_per_million: 1, output_usd_per_million: 5, estimated_pulse_usd: 0.000042, cpa_relevant: true, available_in_cpa: true, eligible: true },
    { provider: "openai", model: "custom", has_price: true, input_usd_per_million: 0, output_usd_per_million: 2, estimated_pulse_usd: 0, available_in_cpa: true, custom: true, eligible: true },
    { provider: "openai", model: "removed", has_price: true, cpa_relevant: true, custom: true, disabled: true, eligible: false },
    { provider: "anthropic", model: "unpriced", cpa_relevant: true, eligible: false, reason: "No price" },
    { provider: "catalog", model: "other", has_price: true, eligible: false, reason: "Unavailable" },
  ];
  const names = ["priceUSD", "filteredPrices", "priceProvider", "priceConfirmed", "priceBadges", "renderPrices", "priceRefreshPaused", "stopPricePolling", "pollPriceRefresh", ...extraNames];
  const ctx = context(names, {
    $, document: { activeElement: null }, prices: { source: "models.dev", fetched_at: "2026-11-25T00:00:00Z", refresh_hours: 24, entries },
    pricesBusy: false, pricesLoading: false, pricesError: "", priceEditing: null, priceEditorDirty: false, rawConfig: {},
    pricesPollTimer: null, pricesPollUntil: 0, setTimeout: () => 1, clearTimeout() {},
    status: { targets: [{ key: "g" }], accounts: [{ provider: "claude", groups: [{ confirmed_model: "haiku" }] }] },
    el: priceNode, clear: node => { node.children = []; node.text = ""; },
    badge: (kind, text) => priceNode("span", { class: "badge badge-" + kind, text }),
    parseTime: value => value ? Date.parse(value) : null, fmtDateTime: value => new Date(value).toISOString(),
    editPrice: () => {}, togglePrice: () => {}, resetPrice: () => {},
    ...globals,
  });
  return { ctx, nodes, $, entries };
}

test("prices render CPA models by default, with fetched, custom, removed, confirmed and ineligible badges", () => {
  const { ctx, $ } = pricingContext();
  ctx.renderPrices();
  const table = $("prices-rows").textContent;
  assert.match(table, /haiku.*\$1.*\$5.*\$0\.000042.*FetchedConfirmed/);
  assert.match(table, /custom.*\$0.*Custom/);
  assert.match(table, /removed.*CustomRemoved.*RestoreReset/);
  assert.match(table, /unpricedUnpricedUnpriced—FetchedIneligible: No price/);
  assert.doesNotMatch(table, /catalog|other/);
  assert.equal($("prices-count").textContent, "4 of 5 models");
  assert.match($("prices-meta").textContent, /models.dev.*2026-11-25/);
  assert.equal($("prices-interval").value, 24);
});

test("confirmed price badges require CPA relevance, availability and the confirming provider family", () => {
  const { ctx, entries } = pricingContext();
  const entry = entries[0];
  assert.equal(ctx.priceConfirmed(entry), true);
  for (const changes of [
    { provider: "reseller", cpa_relevant: false }, { cpa_relevant: undefined },
    { available_in_cpa: false }, { available_in_cpa: undefined }, { provider: "openai" },
    { model: "different" },
  ]) {
    const row = { ...entry, ...changes };
    assert.equal(ctx.priceConfirmed(row), false, JSON.stringify(changes));
    assert.equal(ctx.priceBadges(row).some(node => node.textContent === "Confirmed"), false);
  }
  for (const [accountProvider, catalogProvider, model] of [
    ["codex", "openai", "gpt-test"], ["claude", "anthropic", "claude-test"],
    ["antigravity", "anthropic", "claude-test"], ["antigravity", "google", "gemini-test"],
    ["antigravity", "openai", "gpt-test"], ["Antigravity", "openai", "GPT-test"],
    ["gemini", "google", "gemini-test"], ["kimi", "moonshotai", "kimi-test"], ["grok", "xai", "grok-test"],
  ]) {
    ctx.status.accounts = [{ provider: accountProvider, groups: [{ confirmed_model: model }] }];
    assert.equal(ctx.priceConfirmed({ ...entry, provider: catalogProvider, model }), true);
    assert.equal(ctx.priceBadges({ ...entry, provider: catalogProvider, model }).some(node => node.textContent === "Confirmed"), true);
    assert.equal(ctx.priceConfirmed({ ...entry, provider: "reseller", model }), false);
  }
});

test("All Catalog Models and instant search filter rendered prices by provider or model", () => {
  const { ctx, $ } = pricingContext();
  $("prices-scope").value = "all";
  ctx.renderPrices();
  assert.match($("prices-rows").textContent, /catalogother.*Ineligible: Unavailable/);
  $("prices-search").value = "  ANTHROPIC ";
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 2);
  $("prices-search").value = "OTHER";
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 1);
  assert.match($("prices-rows").textContent, /catalogother/);
  $("prices-search").value = "missing";
  ctx.renderPrices();
  assert.match($("prices-rows").textContent, /No models match/);
  assert.match(source, /\$\("prices-search"\)\.addEventListener\("input", renderPrices\)/);
});

test("prices preserve zero, missing prices, refresh state, errors and a refresh interval draft", () => {
  const { ctx, $ } = pricingContext();
  ctx.prices.refreshing = true;
  ctx.prices.last_error = "Catalog unavailable; cached prices kept";
  ctx.document.activeElement = $("prices-interval");
  $("prices-interval").value = "12";
  ctx.renderPrices();
  assert.equal($("prices-refresh").disabled, true);
  assert.match($("prices-meta").textContent, /Refresh in progress/);
  assert.equal($("prices-error").hidden, false);
  assert.match($("prices-error").textContent, /cached prices kept/);
  assert.equal($("prices-interval").value, "12");
  assert.equal(ctx.priceUSD(0), "$0");
  assert.equal(ctx.priceUSD(undefined), "—");
  assert.equal(ctx.priceUSD(-1), "—");
  assert.equal(ctx.priceUSD(1e-10), "$1.00e-10");
});

test("prices cap filtered matches at 200 and show a hint only when more models match", () => {
  const { ctx, $ } = pricingContext();
  ctx.prices.entries = Array.from({ length: 250 }, (_, i) => ({
    provider: "catalog", model: "model-" + i, cpa_relevant: i < 5, eligible: true,
  }));
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 5);
  assert.equal($("prices-limit").hidden, true);
  $("prices-scope").value = "all";
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 200);
  assert.equal($("prices-count").textContent, "250 of 250 models");
  assert.equal($("prices-limit").hidden, false);
  assert.equal($("prices-limit").textContent, "Showing 200 of 250 models — narrow your search to see more");
  $("prices-search").value = "model-249";
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 1);
  assert.match($("prices-rows").textContent, /model-249/);
  assert.equal($("prices-limit").hidden, true);
  assert.equal($("prices-limit").textContent, "");
  $("prices-search").value = "";
  $("prices-scope").value = "cpa";
  ctx.prices.entries.forEach(entry => { entry.cpa_relevant = true; });
  ctx.renderPrices();
  assert.equal($("prices-rows").children.length, 200);
  assert.equal($("prices-limit").hidden, false);
});

test("prices keep catalog strings as literal DOM text and attributes", () => {
  const { ctx, $ } = pricingContext(["el", "badge"]);
  ctx.document.createElement = tag => {
    const node = priceNode(tag);
    node.addEventListener = (name, callback) => { node.events[name] = callback; };
    Object.defineProperty(node, "innerHTML", { set() { assert.fail("catalog strings must not enter the HTML parser"); } });
    return node;
  };
  ctx.document.createTextNode = text => priceNode("#text", { text });
  const payload = '\"><img src=x onerror=alert(1)>&\'';
  ctx.prices.source = payload;
  ctx.prices.last_error = payload;
  ctx.prices.entries = [{ provider: payload, model: payload, reason: payload, cpa_relevant: true }];
  ctx.renderPrices();
  assert.ok($("prices-meta").textContent.includes(payload));
  assert.equal($("prices-error").textContent, payload);
  const row = $("prices-rows").children[0];
  assert.equal(row.children[0].children[0].children[0].textContent, payload);
  assert.equal(row.children[0].children[0].children[1].textContent, payload);
  assert.ok(row.children[4].textContent.includes("Ineligible: " + payload));
  assert.equal(descendants(row, node => node.tagName === "BUTTON")[0].getAttribute("aria-label"), "Edit " + payload);
  assert.equal(descendants(row, node => node.tagName === "IMG").length, 0);
});

test("prices table buttons open the editor and dispatch remove, restore and reset", () => {
  const actions = [];
  const { ctx, $, entries } = pricingContext([], {
    editPrice: entry => actions.push(["edit", entry.model]),
    togglePrice: entry => actions.push(["toggle", entry.model]),
    resetPrice: entry => actions.push(["reset", entry.model]),
  });
  ctx.renderPrices();
  for (const row of $("prices-rows").children) {
    for (const button of descendants(row, node => node.tagName === "BUTTON")) button.events.click();
  }
  assert.deepEqual(actions, [["edit", "haiku"], ["toggle", "haiku"], ["edit", "custom"], ["toggle", "custom"], ["reset", "custom"],
    ["edit", "removed"], ["toggle", "removed"], ["reset", "removed"], ["edit", "unpriced"], ["toggle", "unpriced"]]);
});

const priceMutationFunctions = ["mutatePrice", "loadPrices", "refreshPrices", "togglePrice", "resetPrice"];

function pricePollingContext(globals = {}) {
  let now = 1000, serial = 0;
  const timers = new Map();
  const result = pricingContext([...priceMutationFunctions, "showView"], {
    Date: { now: () => now }, VIEW_STORE: "view", key: "key", history: null,
    localStorage: { setItem() {} }, pressSegment() {},
    setTimeout: (callback, delay) => { assert.equal(delay, 1000); timers.set(++serial, callback); return serial; },
    clearTimeout: id => timers.delete(id),
    ...globals,
  });
  return { ...result, timers, advance: async () => {
    assert.equal(timers.size, 1);
    const [id, callback] = timers.entries().next().value;
    timers.delete(id); now += 1000;
    await callback();
  }, setTime: value => { now = value; } };
}

test("price refresh polls every second until completion and immediately enables the button", async () => {
  const calls = [];
  let reads = 0;
  const { ctx, $, timers, advance } = pricePollingContext({
    API: "/api", api: async (method, url) => {
      calls.push([method, url]);
      return { entries: [], refreshing: method === "POST" || ++reads < 3 };
    },
  });
  await ctx.refreshPrices();
  assert.equal($("prices-refresh").disabled, true);
  await advance();
  assert.equal($("prices-refresh").disabled, true);
  await advance();
  assert.equal($("prices-refresh").disabled, false);
  assert.equal($("prices-refresh").textContent, "Refresh Prices");
  assert.equal(timers.size, 0);
  assert.deepEqual(calls, [["POST", "/api/prices/refresh"], ...Array.from({ length: 3 }, () => ["GET", "/api/prices"])]);
});

test("price refresh polling stops on view exit, page hide, hidden documents and after two minutes", async () => {
  const { ctx, $, timers, advance, setTime } = pricePollingContext({
    API: "/api", api: async () => ({ entries: [], refreshing: true }),
  });
  await ctx.loadPrices();
  ctx.showView("settings");
  assert.equal(timers.size, 0);
  $("view-prices").hidden = false;
  await ctx.loadPrices();
  const windowEvents = {};
  ctx.window = { addEventListener: (name, fn) => { windowEvents[name] = fn; } };
  vm.runInContext(source.match(/  window.addEventListener\("pagehide", stopPricePolling\);/)[0], ctx);
  windowEvents.pagehide();
  assert.equal(timers.size, 0);
  await ctx.loadPrices();
  ctx.document.hidden = true;
  await advance();
  assert.equal(timers.size, 0);
  ctx.document.hidden = false;
  await ctx.loadPrices();
  setTime(ctx.pricesPollUntil);
  await advance();
  assert.equal(timers.size, 0);
});

test("price refresh polling respects edits and retries while a request is in flight", async () => {
  let reads = 0, reply;
  const { ctx, timers, advance } = pricePollingContext({
    API: "/api", api: () => { reads++; return new Promise(resolve => { reply = resolve; }); },
  });
  ctx.prices.refreshing = true;
  ctx.priceEditorDirty = true;
  ctx.pollPriceRefresh();
  await advance();
  assert.equal(reads, 0);
  ctx.priceEditorDirty = false;
  const request = ctx.loadPrices();
  await advance();
  assert.equal(reads, 1);
  assert.equal(timers.size, 1);
  reply({ entries: [], refreshing: false });
  await request;
  assert.equal(timers.size, 0);
});

test("Refresh Prices posts once, then reads refresh status with the existing API helper", async () => {
  const calls = [];
  const { ctx, $ } = pricingContext(priceMutationFunctions, {
    API: "/api", api: async (...args) => {
      calls.push(plain(args));
      return args[0] === "POST" ? { ok: true, refreshing: true } : { source: "models.dev", entries: [], refreshing: true };
    },
  });
  const request = ctx.refreshPrices();
  assert.equal($("prices-refresh").disabled, true);
  await ctx.refreshPrices();
  await request;
  assert.deepEqual(calls, [["POST", "/api/prices/refresh", {}], ["GET", "/api/prices"]]);
  assert.equal($("prices-refresh").disabled, true);
  assert.equal($("price-save").disabled, false);
});

test("fetched Remove omits prices, Restore deletes its tombstone, and Reset encodes its identity", async () => {
  const calls = [];
  const { ctx, entries } = pricingContext(priceMutationFunctions, {
    API: "/api", api: async (...args) => { calls.push(plain(args)); return { entries: [] }; },
  });
  await ctx.togglePrice(entries[0]);
  assert.deepEqual(calls[0], ["PUT", "/api/prices/override", { provider: "anthropic", model: "haiku", disabled: true }]);
  await ctx.togglePrice({ ...entries[0], disabled: true });
  assert.deepEqual(calls[1], ["DELETE", "/api/prices/override?provider=anthropic&model=haiku", null]);
  await ctx.resetPrice({ provider: "a/b", model: "m +&?" });
  assert.equal(calls[2][0], "DELETE");
  assert.equal(calls[2][1], "/api/prices/override?provider=a%2Fb&model=m%20%2B%26%3F");
});

test("custom Remove and Restore retain custom prices, including zero", async () => {
  const calls = [];
  const { ctx, entries } = pricingContext(priceMutationFunctions, {
    API: "/api", api: async (...args) => { calls.push(plain(args)); return { entries: [] }; },
  });
  for (const disabled of [false, true]) {
    await ctx.togglePrice({ ...entries[1], disabled });
    assert.deepEqual(calls.at(-1), ["PUT", "/api/prices/override", {
      provider: "openai", model: "custom", disabled: !disabled, input_usd_per_million: 0, output_usd_per_million: 2,
    }]);
  }
});

test("a price request failure keeps cached rows, reports the error and enables retry", async () => {
  const { ctx, $ } = pricingContext(priceMutationFunctions, {
    API: "/api", api: async () => { throw new Error("offline"); },
  });
  assert.equal(await ctx.togglePrice(ctx.prices.entries[0]), false);
  assert.match($("prices-error").textContent, /Could not update prices: offline/);
  assert.equal($("prices-refresh").disabled, false);
  assert.match($("prices-rows").textContent, /haiku/);
  await ctx.loadPrices();
  assert.match($("prices-error").textContent, /Could not load prices: offline/);
  assert.equal(ctx.prices.entries.length, 5);
});

function priceForm() {
  const form = priceNode("form");
  form.elements = Object.fromEntries(["provider", "model", "input", "output"].map(name => [name, priceNode("input")]));
  form.reset = () => Object.values(form.elements).forEach(field => { field.value = ""; });
  return form;
}

test("30-second price auto-refresh pauses for search focus, an edited model and unsaved custom input", async () => {
  let calls = 0, tick;
  const form = priceForm();
  const { ctx, $, entries } = pricingContext([...priceMutationFunctions, "editPrice", "clearPriceEditor"], {
    API: "/api", api: async () => { calls++; return { entries }; },
  });
  const original = ctx.$;
  ctx.$ = id => id === "price-editor" ? form : original(id);
  ctx.setInterval = (callback, delay) => { assert.equal(delay, 30000); tick = callback; };
  ctx.loadStatus = async () => {};
  ctx.refreshHistory = () => {};
  vm.runInContext(source.match(/refreshTimer = setInterval\(function \(\) \{[^]*?\}, 30000\);/)[0], ctx);
  const refresh = async () => { tick(); await new Promise(resolve => setImmediate(resolve)); };
  await refresh();
  assert.equal(calls, 1);
  ctx.document.activeElement = $("prices-search");
  await refresh();
  assert.equal(calls, 1);
  ctx.document.activeElement = null;
  ctx.editPrice(entries[0]);
  await refresh();
  assert.equal(calls, 1);
  ctx.clearPriceEditor();
  const listener = source.match(/\$\("price-editor"\)\.addEventListener\("input", function \(\) \{[^]*?\}\);/);
  assert.ok(listener);
  form.addEventListener = (name, callback) => { form.events[name] = callback; };
  vm.runInContext(listener[0], ctx);
  form.elements.provider.value = "draft";
  form.events.input();
  await refresh();
  assert.equal(calls, 1);
  ctx.clearPriceEditor();
  await refresh();
  assert.equal(calls, 2);
});

test("in-flight price reloads discard responses during editing or search", async () => {
  for (const interaction of ["edit", "draft", "search"]) {
    let finish;
    const form = priceForm();
    const { ctx, $, entries } = pricingContext([...priceMutationFunctions, "editPrice", "clearPriceEditor"], {
      API: "/api", api: () => new Promise(resolve => { finish = resolve; }),
    });
    const original = ctx.$;
    ctx.$ = id => id === "price-editor" ? form : original(id);
    const cached = ctx.prices;
    const pending = ctx.loadPrices();
    if (interaction === "edit") {
      ctx.editPrice(entries[0]);
      form.elements.input.value = "9";
    } else if (interaction === "draft") ctx.priceEditorDirty = true;
    else ctx.document.activeElement = $("prices-search");
    finish({ entries: [{ provider: "new", model: "new" }] });
    await pending;
    assert.equal(ctx.prices, cached, interaction);
    assert.equal(ctx.pricesLoading, false);
    if (interaction === "edit") assert.equal(form.elements.input.value, "9");
    ctx.document.activeElement = null;
    ctx.clearPriceEditor();
    const resumed = ctx.loadPrices();
    finish({ entries: [] });
    await resumed;
    assert.equal(ctx.prices.entries.length, 0);
  }
});

test("saving an edited removed model preserves its disabled state", async () => {
  for (const custom of [false, true]) {
    const calls = [], form = priceForm();
    const { ctx, entries } = pricingContext([...priceMutationFunctions, "editPrice", "clearPriceEditor", "savePrice"], {
      API: "/api", api: async (...args) => { calls.push(plain(args)); return { entries: [] }; },
    });
    const original = ctx.$;
    ctx.$ = id => id === "price-editor" ? form : original(id);
    ctx.editPrice({ ...entries[0], disabled: true, custom });
    form.elements.input.value = "2";
    assert.equal(await ctx.savePrice({ preventDefault() {} }), true);
    assert.deepEqual(calls[0], ["PUT", "/api/prices/override", {
      provider: "anthropic", model: "haiku", input_usd_per_million: 2, output_usd_per_million: 5, disabled: true,
    }]);
    assert.equal(ctx.priceEditing, null);
    assert.equal(ctx.priceEditorDirty, false);
  }
});

test("Edit fills a compact price form and saves a numeric override; custom prices accept zero", async () => {
  const calls = [], form = priceForm();
  const { ctx, $, entries } = pricingContext([...priceMutationFunctions, "editPrice", "clearPriceEditor", "savePrice"], {
    API: "/api", api: async (...args) => { calls.push(plain(args)); return { entries: [] }; },
  });
  const nodes = { "price-editor": form };
  const original = ctx.$;
  ctx.$ = id => nodes[id] || original(id);
  ctx.editPrice(entries[0]);
  assert.equal(form.elements.provider.value, "anthropic");
  assert.equal(form.elements.input.value, 1);
  assert.equal(form.elements.input.focused, true);
  assert.equal(form.elements.model.readOnly, true);
  form.elements.output.value = "7.5";
  let prevented = false;
  assert.equal(await ctx.savePrice({ preventDefault: () => { prevented = true; } }), true);
  assert.equal(prevented, true);
  assert.deepEqual(calls[0][2], { provider: "anthropic", model: "haiku", input_usd_per_million: 1, output_usd_per_million: 7.5, disabled: false });
  assert.equal(form.elements.model.readOnly, false);
  form.elements.provider.value = " custom "; form.elements.model.value = " unpriced ";
  form.elements.input.value = "0"; form.elements.output.value = "0";
  await ctx.savePrice({ preventDefault() {} });
  assert.equal(calls[1][2].input_usd_per_million, 0);
  assert.equal(calls[1][2].model, "unpriced");
  assert.match($("prices-save-state").textContent, /Price saved/);
});

test("invalid prices never save, and a rejected price keeps the editor draft", async () => {
  const form = priceForm(); let calls = 0;
  const { ctx, $ } = pricingContext([...priceMutationFunctions, "clearPriceEditor", "savePrice"], {
    API: "/api", api: async () => { calls++; throw new Error("rejected"); },
  });
  const original = ctx.$; ctx.$ = id => id === "price-editor" ? form : original(id);
  form.elements.provider.value = "p"; form.elements.model.value = "m";
  form.elements.input.value = "-1"; form.elements.output.value = "1";
  await ctx.savePrice({ preventDefault() {} });
  assert.equal(calls, 0);
  assert.equal($("price-editor-error").hidden, false);
  form.elements.input.value = "0.5";
  await ctx.savePrice({ preventDefault() {} });
  assert.equal(calls, 1);
  assert.equal(form.elements.input.value, "0.5");
  assert.match($("prices-error").textContent, /rejected/);
});

test("refresh interval uses queued config patches, preserves pricing settings and supports manual only", async () => {
  const writes = [];
  const { ctx, $ } = pricingContext(["savePriceInterval"], {
    rawConfig: { pricing: { extra: true } }, saveQueue: Promise.resolve(), whenApplied: async () => true,
    patchConfig: async patch => { writes.push(plain(patch)); return "token"; },
  });
  $("prices-interval").value = "0";
  await ctx.savePriceInterval();
  assert.deepEqual(writes, [{ pricing: { extra: true, refresh_hours: 0 } }]);
  assert.match($("prices-save-state").textContent, /saved/);
  $("prices-interval").value = "169";
  await ctx.savePriceInterval();
  $("prices-interval").value = "1.5";
  await ctx.savePriceInterval();
  $("prices-interval").value = "";
  await ctx.savePriceInterval();
  assert.equal(writes.length, 1);
});

test("account pulse badges distinguish estimated, confirmed cheapest and manual overrides", () => {
  const { ctx } = pricingContext(["pulseModelInfo", "accountModelKey", "pulseModelControl"], { modelSaves: {} });
  const account = { auth_id: "auth" }, base = { key: "g", available_models: ["cheap", "other"], estimated_cheapest_model: "cheap", estimated_cheapest_cost_usd: 0.000042 };
  let node = ctx.pulseModelControl(account, base);
  assert.match(node.textContent, /Estimated cheapestcheapEst. pulse: \$0.000042Pulse modelAuto \(Cheapest\)/);
  node = ctx.pulseModelControl(account, { ...base, confirmed_model: "cheap" });
  assert.match(node.textContent, /Confirmed cheapest/);
  node = ctx.pulseModelControl(account, { ...base, confirmed_model: "other" });
  assert.match(node.textContent, /Estimated cheapest/);
  node = ctx.pulseModelControl(account, { ...base, confirmed_model: "cheap", manual_model: "manual", last_successful_model: "other" });
  assert.match(node.textContent, /Manual overridemanualLast successful: other/);
  assert.doesNotMatch(node.textContent, /Confirmed cheapest/);
  assert.equal(descendants(node, node => node.tagName === "SELECT")[0].value, "manual");
  assert.equal(ctx.pulseModelControl(account, { key: "old-server" }), null);
});

test("account pulse selector lists available models and dispatches its account and group", () => {
  const calls = [], account = { auth_id: "auth" }, group = { key: "g", available_models: ["a", "b", "a"] };
  const { ctx } = pricingContext(["pulseModelInfo", "accountModelKey", "pulseModelControl"], {
    modelSaves: {}, saveAccountModel: (...args) => calls.push(args),
  });
  const control = ctx.pulseModelControl(account, group);
  const select = descendants(control, node => node.tagName === "SELECT")[0];
  assert.equal(select.children.length, 3);
  assert.equal(select.value, "");
  select.value = "b"; select.events.change();
  assert.deepEqual(calls, [[account, group, "b"]]);
  assert.match(source, /var modelControl = pulseModelControl\(account, group\);/);
});

test("pulse model selectors require a matching target and at least one available model", () => {
  const { ctx } = pricingContext(["pulseModelInfo", "accountModelKey", "pulseModelControl"], { modelSaves: {} });
  const group = { key: "g", available_models: ["model"] };
  assert.ok(ctx.pulseModelControl({ provider: "codex" }, group));
  for (const available_models of [undefined, null, [], "model"]) {
    assert.equal(ctx.pulseModelControl({ provider: "codex" }, { ...group, available_models }), null);
  }
  for (const provider of ["kimi", "xai", "grok", "codex"]) {
    assert.equal(ctx.pulseModelControl({ provider }, { ...group, key: "monitor-only" }), null);
  }
  ctx.status.targets = [];
  assert.equal(ctx.pulseModelControl({ provider: "claude" }, group), null);
  delete ctx.status.targets;
  assert.equal(ctx.pulseModelControl({ provider: "claude" }, group), null);
});

test("queued account model changes preserve other overrides and Auto removes only the chosen group", async () => {
  const writes = [];
  const { ctx } = pricingContext(["accountModelKey", "saveAccountModel"], {
    modelSaves: {}, rawConfig: { account_models: { existing: { g: "stay" }, auth: { other: "keep" } } },
    saveQueue: Promise.resolve(), renderAccounts() {}, whenApplied: async () => true, loadStatus: async () => {},
    patchConfig: async patch => { writes.push(plain(patch)); return "token"; },
  });
  const account = { auth_id: "auth", auth_index: "index", name: "name" }, group = { key: "g" };
  const a = ctx.saveAccountModel(account, group, "cheap");
  const b = ctx.saveAccountModel({ auth_id: "second" }, group, "other");
  await Promise.all([a, b]);
  assert.deepEqual(writes[1].account_models, { existing: { g: "stay" }, auth: { other: "keep", g: "cheap" }, second: { g: "other" } });
  await ctx.saveAccountModel(account, group, "");
  assert.equal(writes[2].account_models.auth.g, "");
  assert.deepEqual(plain(ctx.rawConfig.account_models.auth), { other: "keep" });
  assert.equal(Object.keys(ctx.modelSaves).length, 0);
});

test("account model save errors and delayed application stay visible and allow retry", async () => {
  const { ctx } = pricingContext(["accountModelKey", "saveAccountModel"], {
    modelSaves: {}, saveQueue: Promise.resolve(), renderAccounts() {}, whenApplied: async () => false,
    patchConfig: async () => { throw new Error("offline"); },
  });
  const account = { auth_index: "index" }, group = { key: "g", manual_model: "old" };
  await ctx.saveAccountModel(account, group, "new");
  const key = ctx.accountModelKey(account, group);
  assert.equal(ctx.modelSaves[key].pending, false);
  assert.equal(ctx.modelSaves[key].value, "old");
  assert.match(ctx.modelSaves[key].message, /offline/);
  ctx.patchConfig = async () => "token";
  await ctx.saveAccountModel(account, group, "new");
  assert.match(ctx.modelSaves[key].message, /waiting for the plugin/);
  assert.equal(ctx.modelSaves[key].pending, false);
});

test("pricing view navigation preserves existing settings and only draws visible charts", () => {
  const nodes = Object.fromEntries(["quota", "settings", "prices"].map(view => ["view-" + view, {}]));
  const pressed = [], stored = {}, loaded = [], charts = [];
  const ctx = context(["showView"], {
    $: id => nodes[id], VIEW_STORE: "view", key: "key", history: {}, status: {},
    localStorage: { setItem: (key, value) => { stored[key] = value; } },
    pressSegment: (...args) => pressed.push(args), renderChart: () => charts.push(true), loadPrices: () => loaded.push(true), stopPricePolling() {},
  });
  ctx.showView("prices", true);
  assert.equal(nodes["view-prices"].hidden, false);
  assert.equal(nodes["view-quota"].hidden, true);
  assert.equal(nodes["view-settings"].hidden, true);
  assert.equal(loaded.length, 1);
  assert.equal(charts.length, 0);
  ctx.showView("settings");
  assert.equal(nodes["view-settings"].hidden, false);
  ctx.showView("invalid");
  assert.equal(nodes["view-quota"].hidden, false);
  assert.equal(charts.length, 1);
  assert.equal(stored.view, "quota");
});

function windowContext(names, globals = {}) {
  const ctx = context(["translate", ...names], globals);
  vm.runInContext(source.slice(source.indexOf("  var I18N ="), source.indexOf("\n  function translate(")), ctx);
  for (const name of ["RANGES", "DEFAULT_RANGE", "WINDOW_SECONDS"]) {
    vm.runInContext(source.match(new RegExp("  var " + name + " = [^]*?;"))[0], ctx);
  }
  return ctx;
}

test("Restore on a removed unpriced model deletes its override and keeps the model unpriced", async () => {
  const calls = [];
  const { ctx } = pricingContext(priceMutationFunctions, {
    API: "/api", api: async (...args) => { calls.push(plain(args)); return { entries: [{ provider: "a/b", model: "m +&?", has_price: false }] }; },
  });
  assert.equal(await ctx.togglePrice({ provider: "a/b", model: "m +&?", disabled: true, has_price: false }), true);
  assert.deepEqual(calls, [["DELETE", "/api/prices/override?provider=a%2Fb&model=m%20%2B%26%3F", null]]);
  assert.equal(ctx.prices.entries[0].has_price, false);
  await ctx.togglePrice({ provider: "p", model: "m", has_price: false });
  assert.deepEqual(calls[1], ["PUT", "/api/prices/override", { provider: "p", model: "m", disabled: true }]);
});

function windowSettingsContext() {
  const checks = ["short", "long"].map(value => {
    const input = priceNode("input", { name: "ignition.windows", value });
    input.type = "checkbox";
    return input;
  });
  const group = { querySelectorAll: () => checks };
  checks.forEach(input => { input.closest = selector => selector.includes(".window-checks") ? group : null; });
  const other = priceNode("input", { name: "ignition.start_hour", value: "7" });
  other.type = "number";
  const inputs = checks.concat(other);
  const ctx = context(["fillInput", "getPath", "setPath", "topKey", "inputTop", "captureIntent", "buildPatch", "noteEdit", "saveControls"], {
    $: () => ({ elements: inputs }), rawConfig: { ignition: { hidden_option: "keep", start_hour: 8 } },
    status: { config: { ignition: { windows: ["short", "long"] } } },
  });
  return { ctx, checks, inputs };
}

test("ignition window settings load defaults, null, empty and each explicit class selection", () => {
  const { ctx, checks } = windowSettingsContext();
  const cases = [[{}, [true, true]], [{ windows: null }, [true, true]], [{ windows: [] }, [false, false]],
    [{ windows: ["short"] }, [true, false]], [{ windows: ["long"] }, [false, true]], [{ windows: ["short", "long"] }, [true, true]]];
  for (const [ignition, expected] of cases) {
    checks.forEach(input => { input.checked = !expected[checks.indexOf(input)]; ctx.fillInput(input, { ignition }); });
    assert.deepEqual(checks.map(input => input.checked), expected);
    const patch = plain(ctx.buildPatch("ignition"));
    assert.deepEqual(patch.ignition.windows, ["short", "long"].filter((value, i) => expected[i]));
    assert.equal(patch.ignition.hidden_option, "keep");
    assert.equal(patch.ignition.start_hour, 7);
  }
  checks.forEach(input => ctx.fillInput(input, {}));
  assert.ok(checks.every(input => input.checked));
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /name="ignition.windows" type="checkbox" value="short" checked/);
  assert.match(html, /name="ignition.windows" type="checkbox" value="long" checked/);
  assert.match(html, /Short windows \(5h\)/);
  assert.match(html, /Long windows \(7d \/ 30d\)/);
});

test("explicit empty ignition windows survive status and config JSON and leave both choices unchecked", () => {
  const { checks } = windowSettingsContext();
  const ctx = context(["effectiveConfig", "fillInput", "getPath"], {
    status: JSON.parse('{"config":{"ignition":{"windows":[]}}}'),
    rawConfig: JSON.parse('{"ignition":{"windows":[]}}'), MCP_LISTEN: "127.0.0.1:8318",
  });
  const effective = ctx.effectiveConfig();
  assert.deepEqual(plain(effective.ignition.windows), []);
  for (const check of checks) {
    check.checked = true;
    ctx.fillInput(check, effective);
  }
  assert.deepEqual(checks.map(check => check.checked), [false, false]);
});

test("queued ignition window saves keep their captured selections and peer edit generations", () => {
  const { ctx, checks } = windowSettingsContext();
  checks[0].checked = true; checks[1].checked = false;
  ctx.noteEdit(checks[0]);
  const first = ctx.captureIntent(["ignition"]);
  checks[0].checked = false; checks[1].checked = true;
  ctx.noteEdit(checks[1]);
  const second = ctx.captureIntent(["ignition"]);
  assert.deepEqual(plain(ctx.buildPatch("ignition", first)).ignition.windows, ["short"]);
  assert.deepEqual(plain(ctx.buildPatch("ignition", second)).ignition.windows, ["long"]);
  assert.equal(first.submitted.includes(checks[0]), true);
  assert.equal(first.submitted.includes(checks[1]), true);
  assert.equal(second.states[0].edit, first.states[0].edit + 1);
  assert.equal(second.states[1].edit, first.states[1].edit + 1);
});

function pulseWindowsContext(extraNames = [], globals = {}) {
  return windowContext(["parseTime", "targetStatus", "pulseWindowStatus", "pulseWindowControl", ...extraNames], {
    status: { targets: [] }, el: priceNode,
    badge: (kind, text) => priceNode("span", { class: "badge badge-" + kind, text }),
    maskedNodes: text => [priceNode("span", { text })],
    fmtDateTime: time => time.toISOString(), fmtDateTimeDay: time => time.toISOString(), fmtIn: () => "in 1 h",
    ...globals,
  });
}

test("window badges show active, due, awaiting confirmation and disabled with target failure precedence", () => {
  const ctx = pulseWindowsContext(), future = new Date(Date.now() + DAY * 1000).toISOString();
  const past = new Date(Date.now() - DAY * 1000).toISOString();
  const active = { enabled: true, next_due: future, reset: future };
  assert.equal(ctx.pulseWindowStatus(active, {}).text, "Active");
  assert.equal(ctx.pulseWindowStatus({ ...active, next_due: past }, {}).text, "Due");
  assert.equal(ctx.pulseWindowStatus({ enabled: true, rolling: true }, {}).text, "Due");
  assert.equal(ctx.pulseWindowStatus({ enabled: true, reset: past }, {}).text, "Due");
  assert.equal(ctx.pulseWindowStatus({ ...active, last_attempt: past }, {}).text, "Awaiting confirmation");
  assert.equal(ctx.pulseWindowStatus({ ...active, last_attempt: future, last_success: past }, {}).text, "Awaiting confirmation");
  assert.equal(ctx.pulseWindowStatus({ ...active, last_attempt: past, last_success: past }, {}).text, "Active");
  assert.equal(ctx.pulseWindowStatus({ ...active, last_attempt: past, last_success: future }, {}).text, "Active");
  assert.equal(ctx.pulseWindowStatus({ ...active, enabled: false, last_attempt: past }, {}).text, "Disabled");
  assert.equal(ctx.pulseWindowStatus({ ...active, last_attempt: past }, { consecutive_failures: 2 }).text, "2 failures");
  assert.match(ctx.pulseWindowStatus(active, { circuit_until: future, circuit_reason: "CPA cooldown" }).text, /Paused until/);
  const blocked = ctx.pulseWindowStatus({ ...active, last_attempt: past }, { blocked_window: "30d", blocked_until: future });
  assert.equal(blocked.text, "30-day quota used up");
  assert.match(blocked.title, /Ignition resumes after the reset/);
  assert.equal(ctx.pulseWindowStatus(active, null).text, "Monitor-only");
});

test("long windows with no reset or due time show a muted unknown badge", () => {
  const ctx = pulseWindowsContext([], { status: { targets: [{ key: "g", windows: [{ kind: "30d", enabled: true }] }] } });
  const state = ctx.pulseWindowStatus({ kind: "30d", enabled: true }, {});
  assert.equal(state.kind, "muted");
  assert.equal(state.text, "Reset unknown");
  const box = ctx.pulseWindowControl({}, { key: "g" });
  const badges = descendants(box, node => node.getAttribute("class") === "badge badge-muted");
  assert.equal(badges.length, 1);
  assert.equal(badges[0].textContent, "30d · Reset unknown");
  assert.doesNotMatch(box.textContent, /Active/);
  assert.equal(ctx.pulseWindowStatus({ kind: "30d", enabled: true, rolling: true }, {}).text, "Due");
});

test("pending attempts with unknown resets show a muted Pending badge", () => {
  const past = new Date(Date.now() - DAY * 1000).toISOString();
  const future = new Date(Date.now() + DAY * 1000).toISOString();
  const ctx = pulseWindowsContext();
  for (const kind of ["5h", "7d", "30d"]) {
    for (const next_due of [undefined, future]) {
      const w = { kind, enabled: true, last_attempt: past, next_due };
      assert.deepEqual(plain(ctx.pulseWindowStatus(w, {})), { kind: "muted", text: "Pending" });
      ctx.status.targets = [{ key: "g", windows: [w] }];
      const box = ctx.pulseWindowControl({}, { key: "g" });
      assert.equal(descendants(box, node => node.getAttribute("class") === "badge badge-muted")[0].textContent, kind + " · Pending");
      assert.doesNotMatch(box.textContent, /Awaiting confirmation/);
      assert.equal(ctx.pulseWindowStatus(w, { consecutive_failures: 1 }).kind, "warning");
    }
  }
});

test("account group windows join targets by key, show each timestamp and keep one manual pulse", () => {
  const future = new Date(Date.now() + DAY * 1000).toISOString(), calls = [], confirms = [];
  const group = { key: "g", windows: [{ id: "fallback", kind: "7d" }] };
  const target = { key: "g", label: "Account", windows: [
    { id: "window-5h", kind: "5h", class: "short", enabled: true, reset: future, next_due: future },
    { id: "window-30d", kind: "30d", class: "long", enabled: false, rolling: true },
  ] };
  const ctx = pulseWindowsContext([], {
    API: "/api", status: { targets: [{ key: "other", windows: [{ id: "wrong" }] }, target] },
    confirm: value => { confirms.push(value); return true; }, runAction: (...args) => calls.push(args),
  });
  const box = ctx.pulseWindowControl({ cooldown_until: future }, group);
  assert.match(box.textContent, /5h · ActiveReset: .*Next action:/);
  assert.match(box.textContent, /30d · DisabledStarts with use/);
  assert.match(box.textContent, /CPA cooldown until/);
  assert.doesNotMatch(box.textContent, /fallback|wrong/);
  assert.equal(box.children[0].getAttribute("data-window-class"), "short");
  assert.equal(box.children[1].getAttribute("data-window-class"), "long");
  const buttons = descendants(box, node => node.tagName === "BUTTON");
  assert.equal(buttons.length, 1);
  buttons[0].events.click();
  assert.equal(confirms.length, 1);
  assert.equal(calls[0][1], "/api/ignite");
  assert.deepEqual(plain(calls[0][2]), { target: "g" });
  ctx.confirm = () => false;
  buttons[0].events.click();
  assert.equal(calls.length, 1);
  assert.match(source, /block.appendChild\(pulseWindowControl\(account, group\)\)/);
  const renderTargets = source.match(/  function renderTargets\([^]*?\n  }/)[0];
  assert.doesNotMatch(renderTargets, /\/ignite|ignite_now/);
});

test("Kimi and xAI group windows without targets show monitor-only and no manual pulse", () => {
  const ctx = pulseWindowsContext();
  for (const provider of ["kimi", "xai"]) {
    const box = ctx.pulseWindowControl({ provider }, { key: provider, windows: [
      { id: "long", kind: "30d", reset: "2026-12-25T00:00:00Z" }, { id: "short", kind: "5h", rolling: true },
    ] });
    assert.match(box.textContent, /30d · Monitor-onlyReset: 2026-12-25T00:00:00.000Z/);
    assert.match(box.textContent, /5h · Monitor-onlyStarts with use/);
    assert.equal(descendants(box, node => node.tagName === "BUTTON").length, 0);
  }
});

test("30d quota labels, defaults and chart ranges stay distinct from the calendar month range", () => {
  const ctx = windowContext(["windowName", "rangeFor", "extendsAhead", "eventWindow"], {
    rememberChart: () => false,
  });
  assert.equal(ctx.windowName({ kind: "30d", short: "Monthly", label: "Monthly" }), "30-day window");
  assert.equal(ctx.windowName({ short: "30d" }), "30-day window");
  assert.equal(ctx.windowName({ short: "5h" }), "5-hour window");
  assert.equal(ctx.windowName({ short: "7d" }), "7-day window");
  assert.equal(ctx.windowName({ kind: "7d", short: "Opus 7d", label: "7 Day Opus" }), "7 Day Opus");
  assert.equal(ctx.rangeFor("30d"), "31d");
  assert.equal(ctx.rangeFor("7d"), "8d");
  assert.equal(ctx.rangeFor("5h"), "6h");
  assert.deepEqual(plain(ctx.RANGES["30d"].map(range => range[0])), ["24h", "15d", "31d", "61d", "90d", "151d"]);
  assert.equal(ctx.WINDOW_SECONDS["30d"], 30 * DAY);
  assert.equal(ctx.extendsAhead("30d", "15d"), false);
  assert.equal(ctx.extendsAhead("30d", "31d"), true);
  assert.ok(ctx.RANGES["7d"].some(range => range[0] === "1mo" && range[2] === "mo"));
  assert.equal(ctx.translate("quota_30d"), "30-day quota");
  assert.equal(ctx.translate("legend_projection_30d"), "Projection at this cycle's daily average");
  assert.deepEqual(plain(ctx.eventWindow({ window: "thirty-day" })), { kind: "30d", label: "30d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "quota-30d" })), { kind: "30d", label: "30d" });
  assert.match(fs.readFileSync(path.join(__dirname, "page.html"), "utf8"), /data-value="30d"[^>]*data-i18n="quota_30d">30-day quota/);
});

function historyWindowContext(extraNames = [], globals = {}) {
  return windowContext(["historyKey", "fetchHistory", "showHistory", "loadHistory", "rangeFor", "windowKind", "recentSamples", "windowKinds", ...extraNames], {
    API: "/v0/management/cpa-pulse", status: { accounts: [] },
    chartWindow: "30d", range: "31d", chartSort: "default", history: null, historyWindow: "", historyRange: "",
    historyCache: {}, historyPending: {}, historySeq: 0, historyTicket: 0, historyLoading: "", historyFailed: "",
    rememberChart: () => false, renderChart() {}, $: () => ({ classList: { add() {} } }),
    ...globals,
  });
}

test("30d chart requests its chosen range and caches independently from 5h and 7d", async () => {
  const calls = [], renders = [], replies = [];
  const ctx = historyWindowContext([], {
    api: (...args) => { calls.push(args); return new Promise(resolve => replies.push(resolve)); },
  });
  ctx.renderChart = () => renders.push([ctx.historyWindow, ctx.historyRange, ctx.history]);
  const request = ctx.loadHistory();
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], "GET");
  const url = new URL(calls[0][1], "https://example.com");
  assert.equal(url.pathname, "/v0/management/cpa-pulse/history");
  assert.equal(url.searchParams.get("window"), "30d");
  assert.equal(url.searchParams.get("range"), "31d");
  const data = { series: [{ kind: "30d", points: [[1, 90, 0]] }] };
  const duplicate = ctx.fetchHistory("30d", "31d");
  assert.equal(calls.length, 1);
  replies.shift()(data);
  assert.equal(await request, true);
  await duplicate;
  assert.equal(ctx.historyWindow, "30d");
  assert.equal(ctx.historyRange, "31d");
  assert.equal(ctx.historyCache["30d|31d"].data, data);
  assert.equal(ctx.historyCache["7d|31d"], undefined);
  assert.equal(ctx.historyCache["5h|31d"], undefined);
  ctx.history = null; ctx.historyWindow = "7d";
  const cached = ctx.loadHistory();
  assert.equal(ctx.history, data);
  assert.deepEqual(renders.at(-1).slice(0, 2), ["30d", "31d"]);
  replies.shift()(data);
  await cached;
  ctx.range = "61d";
  const differentRange = ctx.loadHistory();
  replies.shift()({ series: [] });
  await differentRange;
  assert.ok(ctx.historyCache["30d|31d"]);
  assert.ok(ctx.historyCache["30d|61d"]);
});

test("30d history cannot be replaced by a late response for a different window", async () => {
  const replies = {};
  const ctx = historyWindowContext([], { api: (method, url) => new Promise(resolve => { replies[url] = resolve; }) });
  ctx.chartWindow = "7d"; ctx.range = "8d";
  const old = ctx.loadHistory();
  ctx.chartWindow = "30d"; ctx.range = "31d";
  const chosen = ctx.loadHistory();
  replies["/v0/management/cpa-pulse/history?range=31d&window=30d"]({ series: [{ kind: "30d" }] });
  assert.equal(await chosen, true);
  replies["/v0/management/cpa-pulse/history?range=8d&window=7d"]({ series: [{ kind: "7d" }] });
  assert.equal(await old, false);
  assert.equal(ctx.historyWindow, "30d");
  assert.ok(ctx.historyCache["7d|8d"]);
});

test("30d chart remains selectable from account windows or recent cached history and prefetches", async () => {
  const ctx = historyWindowContext(["prefetchHistory"], {
    status: { accounts: [{ groups: [{ windows: [{ kind: "30d" }, { kind: "unknown" }] }] }] },
    renderWindowButtons: () => false,
  });
  assert.deepEqual(plain(ctx.windowKinds()), ["30d"]);
  ctx.status.accounts[0].groups[0].windows = [{ kind: "5h" }];
  ctx.historyCache["30d|31d"] = { data: { from: "2026-11-01T00:00:00Z", series: [{ kind: "30d", points: [[Date.parse("2026-11-24T00:00:00Z") / 1000, 60, 0]] }] } };
  assert.deepEqual(plain(ctx.windowKinds()), ["5h", "30d"]);
  const fetched = [];
  ctx.fetchHistory = async (kind, range) => { fetched.push([kind, range]); };
  ctx.chartWindow = "5h";
  ctx.prefetchHistory();
  assert.deepEqual(fetched, [["7d", "8d"]]);
  ctx.historyCache = {};
  fetched.length = 0;
  ctx.prefetchHistory();
  assert.deepEqual(fetched, [["7d", "8d"], ["30d", "31d"]]);
});

test("30d burn projection and even pace use a fixed 30-day cycle while 5h keeps its current rate", () => {
  const ctx = windowContext(["cycleRate", "projection", "rowPace", "paceSegments", "paceExtensions", "pooledPace", "paceAt", "extendedPace"], {
    historyWindow: "30d", RESET_DRIFT: 60, RESET_JUMP: 5,
    currentRate: () => ({ rate: -0.01, held: false }),
  });
  const now = 4000 * DAY, reset = now + 20 * DAY;
  const row = { weight: 1, reset, points: [[now, 60]], floor: [[now, 60]], resets: [[now - DAY, reset]] };
  assert.ok(Math.abs(ctx.cycleRate(row, now) + 40 / (10 * DAY)) < 1e-12);
  assert.ok(Math.abs(ctx.rowPace(row, now).at(now) - 100 * 20 / 30) < 1e-9);
  let projected = ctx.projection(row, { now, to: reset }, { end: now }, 600);
  assert.ok(Math.abs(projected.at(now + DAY) - 56) < 1e-9);
  assert.ok(Math.abs(projected.out - (now + 15 * DAY)) < 1e-9);
  ctx.historyWindow = "7d";
  row.reset = now + 5 * DAY;
  assert.ok(Math.abs(ctx.cycleRate(row, now) + 40 / (2 * DAY)) < 1e-12);
  ctx.historyWindow = "5h";
  projected = ctx.projection(row, { now, to: now + 3600 }, { end: now }, 600);
  assert.equal(projected.at(now + 1000), 50);
});

test("changing ignition windows saves through the normal config queue, including both unchecked", async () => {
  const writes = [], { checks, inputs } = windowSettingsContext();
  const ctx = context(["saveField", "captureIntent", "buildPatch", "noteEdit", "saveControls", "inputTop", "topKey", "setPath", "patchConfig"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig: { ignition: { hidden: true } }, status: { config: {} }, CONFIG_API: "/config", savedTokens: [],
    newConfigToken: () => "token", saveHost: () => ({}), claimState: () => () => true,
    api: async (...args) => { writes.push(plain(args)); return {}; },
    showApplied: async () => false, loadConfig: async () => {}, renderIgnitionOff() {},
  });
  for (const selected of [[], ["short"], ["long"], ["short", "long"]]) {
    checks.forEach(input => { input.checked = selected.includes(input.value); });
    ctx.saveField({ target: checks[0] });
    await ctx.saveQueue;
    const [method, url, patch] = writes.at(-1);
    assert.equal(method, "PATCH");
    assert.equal(url, "/config");
    assert.deepEqual(patch.ignition.windows, selected);
    assert.equal(patch.ignition.hidden, true);
  }
});

test("30d chart selection and range buttons request 31d by default and switch to 61d", async () => {
  const calls = [], boxes = {}, pressed = [];
  const node = id => boxes[id] || (boxes[id] = priceNode("div"));
  node("chart").classList = { add() {} };
  node("range-buttons").querySelectorAll = () => node("range-buttons").children;
  node("range-buttons").removeChild = child => { node("range-buttons").children = node("range-buttons").children.filter(value => value !== child); };
  for (const id of ["window-buttons", "range-buttons"]) {
    node(id).addEventListener = (event, fn) => { node(id).events[event] = fn; };
  }
  const ctx = historyWindowContext(["bindSegmented", "renderRangeButtons"], {
    $: node, el: priceNode, chartWindow: "5h", range: "6h", api: async (method, url) => { calls.push([method, url]); return { series: [] }; },
    pressSegment: (...args) => pressed.push(args), saveChartFilters() {}, observeOptions() {}, thumbObserver: { unobserve() {} },
  });
  for (const id of ["window-buttons", "range-buttons"]) {
    vm.runInContext(source.match(new RegExp('  bindSegmented\\("' + id + '", function \\(value\\) \\{[^]*?\\n  \\}\\);'))[0], ctx);
  }
  const windowButton = priceNode("button", { "data-value": "30d", "aria-pressed": "false" });
  windowButton.closest = () => windowButton;
  node("window-buttons").events.click({ target: windowButton });
  await Promise.resolve(); await Promise.resolve();
  assert.equal(ctx.chartWindow, "30d");
  assert.equal(ctx.range, "31d");
  assert.deepEqual(node("range-buttons").children.map(button => button.getAttribute("data-value")), ["24h", "15d", "31d", "61d", "90d", "151d"]);
  assert.deepEqual(node("range-buttons").children.map(button => button.textContent), ["24h", "15d", "31d", "61d", "90d", "151d"]);
  assert.deepEqual(calls[0], ["GET", "/v0/management/cpa-pulse/history?range=31d&window=30d"]);
  const rangeButton = node("range-buttons").children[3];
  rangeButton.closest = () => rangeButton;
  node("range-buttons").events.click({ target: rangeButton });
  await Promise.resolve(); await Promise.resolve();
  assert.equal(ctx.range, "61d");
  assert.deepEqual(calls[1], ["GET", "/v0/management/cpa-pulse/history?range=61d&window=30d"]);
  assert.ok(ctx.historyCache["30d|31d"]);
  assert.ok(ctx.historyCache["30d|61d"]);
});

test("remembered 30d chart filters restore and save their own range without changing calendar month history", () => {
  let saved = { window: "30d", sort: "low", ranges: { "5h": "3h", "7d": "1mo", "30d": "90d" } };
  const ctx = historyWindowContext(["restoreChartFilters", "storedChartFilters", "saveChartFilters"], {
    chartWindow: "5h", range: "6h", CHART_STORE: "filters", rememberChart: () => true,
    restoreChartExpanded() {}, renderRangeButtons() {}, pressSegment() {},
    status: { accounts: [{ groups: [{ windows: [{ kind: "30d" }] }] }] },
    localStorage: { getItem: () => JSON.stringify(saved), setItem: (key, value) => { assert.equal(key, "filters"); saved = JSON.parse(value); } },
  });
  ctx.restoreChartFilters();
  assert.equal(ctx.chartWindow, "30d");
  assert.equal(ctx.range, "90d");
  assert.equal(ctx.chartSort, "low");
  ctx.range = "31d";
  ctx.saveChartFilters();
  assert.deepEqual(saved, { window: "30d", sort: "low", ranges: { "5h": "3h", "7d": "1mo", "30d": "31d" } });
  saved.ranges["30d"] = "1mo";
  assert.equal(ctx.rangeFor("30d"), "31d");
  assert.equal(ctx.rangeFor("7d"), "1mo");
});

test("long history ranges use readable date tick counts through 151 days", () => {
  const ctx = windowContext(["chartTicks"], { offsetSeconds: () => 0, fmtDay: () => "date", unixDate: value => new Date(value * 1000) });
  for (const days of [31, 61, 90, 151]) {
    const ticks = ctx.chartTicks({ from: 0, to: days * DAY });
    assert.ok(ticks.list.length >= 5 && ticks.list.length <= 8, days + " days has " + ticks.list.length + " ticks");
    assert.equal(ticks.label(ticks.list[0]), "date");
  }
});

test("rendered account cards keep a single pulse action for each matching group across short and long windows", () => {
  const cards = priceNode("div"), future = new Date(Date.now() + DAY * 1000).toISOString();
  const windows = [
    { id: "short", kind: "5h", class: "short", enabled: true, reset: future },
    { id: "long", kind: "7d", class: "long", enabled: true, reset: future },
    { id: "longer", kind: "30d", class: "long", enabled: false, reset: future },
  ];
  const ctx = pulseWindowsContext(["renderAccounts"], {
    $: () => cards, document: { activeElement: null }, accountDrag: null, accountsBehind: false, masked: false, SENSITIVE: "sensitive",
    status: { accounts: [{ name: "account", provider: "codex", groups: [{ key: "a", windows: [] }, { key: "b", windows: [] }] }],
      targets: [{ key: "a", label: "A", windows }, { key: "b", label: "B", windows }] },
    renderAccountSettings() {}, accountWeight: () => 1, providerTitle: value => value,
    quotaRow: () => priceNode("div"), pulseModelControl: () => null, freshness: () => null,
    clear: node => { node.children = []; }, snapshotButton: () => priceNode("button"),
  });
  ctx.renderAccounts();
  const groups = descendants(cards, node => node.getAttribute("class") === "quota-group");
  assert.equal(groups.length, 2);
  for (const group of groups) {
    assert.equal(descendants(group, node => node.tagName === "BUTTON").length, 1);
    assert.equal(descendants(group, node => node.getAttribute("class") === "pulse-window").length, 3);
    assert.match(group.textContent, /5h · Active.*7d · Active.*30d · Disabled/);
  }
});
