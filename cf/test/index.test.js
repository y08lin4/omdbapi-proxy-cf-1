import assert from "node:assert/strict";
import http from "node:http";
import test from "node:test";
import { once } from "node:events";
import worker, { buildTargetURL, classifyUpstreamFailure, parseKeys } from "../src/index.js";

async function listen(server) {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const { port } = server.address();
  return `http://127.0.0.1:${port}/`;
}

async function close(server) {
  server.close();
  await once(server, "close");
}

class MemoryKV {
  constructor() {
    this.map = new Map();
  }
  async get(key) {
    return this.map.has(key) ? this.map.get(key) : null;
  }
  async put(key, value) {
    this.map.set(key, String(value));
  }
}

class FailingKV extends MemoryKV {
  async get(key) {
    if (key === "omdb:keys") return null;
    throw new Error("kv unavailable");
  }
  async put() {
    throw new Error("kv unavailable");
  }
}

class MemoryD1 {
  constructor() {
    this.daily = new Map();
    this.hourly = new Map();
  }
  prepare(sql) {
    return new MemoryD1Statement(this, sql);
  }
  async batch(statements) {
    return Promise.all(statements.map((statement) => statement.run()));
  }
}

class MemoryD1Statement {
  constructor(db, sql) {
    this.db = db;
    this.sql = sql;
    this.args = [];
  }
  bind(...args) {
    this.args = args;
    return this;
  }
  async run() {
    if (this.sql.includes("daily_stats")) upsertStat(this.db.daily, this.args);
    if (this.sql.includes("hourly_stats")) upsertStat(this.db.hourly, this.args);
    return { success: true };
  }
  async first() {
    if (this.sql.includes("WHERE day")) return this.db.daily.get(this.args[0]) || null;
    if (this.sql.includes("SUM(total)")) return sumStats(this.db.daily);
    return null;
  }
  async all() {
    const map = this.sql.includes("hourly_stats") ? this.db.hourly : this.db.daily;
    const keyName = this.sql.includes("hourly_stats") ? "hour" : "day";
    const results = [...map.entries()]
      .sort((a, b) => b[0].localeCompare(a[0]))
      .slice(0, this.sql.includes("LIMIT 7") ? 7 : 24)
      .map(([key, value]) => ({ [keyName]: key, ...value }));
    return { results };
  }
}

function upsertStat(map, args) {
  const [key, success, failed, updatedAt] = args;
  const old = map.get(key) || { total: 0, success: 0, failed: 0, updated_at: "" };
  map.set(key, {
    total: old.total + 1,
    success: old.success + Number(success || 0),
    failed: old.failed + Number(failed || 0),
    updated_at: updatedAt
  });
}

function sumStats(map) {
  const total = { total: 0, success: 0, failed: 0 };
  for (const value of map.values()) {
    total.total += value.total;
    total.success += value.success;
    total.failed += value.failed;
  }
  return total;
}

function testEnv(baseURL, extra = {}) {
  return {
    CLIENT_KEYS: "client-good",
    OMDB_KEYS: "bad\ngood",
    ADMIN_KEY: "admin-secret",
    OMDB_API_URL: baseURL,
    OMDB_POSTER_URL: new URL("/poster", baseURL).toString(),
    KEY_COOLDOWN: "1m",
    HTTP_TIMEOUT: "2s",
    CORS_ORIGIN: "",
    ...extra
  };
}

test("parseKeys 支持换行、逗号、空格、注释和去重", () => {
  assert.deepEqual(parseKeys("a,b c\n#x\na"), ["a", "b", "c"]);
});

test("buildTargetURL 会替换客户端 apikey", () => {
  const target = buildTargetURL("https://www.omdbapi.com/", "https://proxy.test/?apikey=client&t=Inception", "omdb");
  assert.equal(target, "https://www.omdbapi.com/?t=Inception&apikey=omdb");
});

test("classifyUpstreamFailure 识别额度错误", () => {
  const body = new TextEncoder().encode('{"Response":"False","Error":"Request limit reached!"}');
  const failure = classifyUpstreamFailure(200, body, "application/json");
  assert.equal(failure.retry, true);
  assert.equal(failure.reason, "quota");
});

test("没有客户端 key 禁止请求", async () => {
  const upstream = http.createServer(() => {
    throw new Error("upstream should not be called");
  });
  const base = await listen(upstream);
  try {
    const response = await worker.fetch(new Request("https://proxy.test/?t=Inception"), testEnv(base));
    assert.equal(response.status, 401);
    assert.deepEqual(await response.json(), { Response: "False", Error: "Invalid API key." });
  } finally {
    await close(upstream);
  }
});

test("OMDb key 额度错误时自动切换下一个 key", async () => {
  const seenKeys = [];
  const upstream = http.createServer((req, res) => {
    const url = new URL(req.url, "http://upstream.test");
    const key = url.searchParams.get("apikey");
    seenKeys.push(key);
    res.setHeader("content-type", "application/json");
    if (key === "bad") {
      res.end(JSON.stringify({ Response: "False", Error: "Request limit reached!" }));
      return;
    }
    res.end(JSON.stringify({ Response: "True", Title: url.searchParams.get("t"), UsedKey: key, ClientKeyWasForwarded: key === "client-good" }));
  });
  const base = await listen(upstream);
  try {
    const response = await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), testEnv(base));
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.Title, "Inception");
    assert.equal(json.UsedKey, "good");
    assert.equal(json.ClientKeyWasForwarded, false);
    assert.deepEqual(seenKeys, ["bad", "good"]);
  } finally {
    await close(upstream);
  }
});

test("普通业务错误不切换 key", async () => {
  let calls = 0;
  const upstream = http.createServer((req, res) => {
    calls += 1;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ Response: "False", Error: "Movie not found!" }));
  });
  const base = await listen(upstream);
  try {
    const response = await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=NoSuchMovie"), testEnv(base, { OMDB_KEYS: "k1\nk2" }));
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { Response: "False", Error: "Movie not found!" });
    assert.equal(calls, 1);
  } finally {
    await close(upstream);
  }
});

test("metrics 统计今日和总请求", async () => {
  const upstream = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ Response: "True", Title: "Inception" }));
  });
  const base = await listen(upstream);
  const env = testEnv(base, { OMDB_KEYS: "only-good" });
  try {
    await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), env);
    const response = await worker.fetch(new Request("https://proxy.test/metrics"), env);
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.requests.today, 1);
    assert.equal(json.requests.total, 1);
  } finally {
    await close(upstream);
  }
});


test("KV 持久化统计今日和总请求", async () => {
  const upstream = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ Response: "True", Title: "Inception" }));
  });
  const base = await listen(upstream);
  const env = testEnv(base, { OMDB_KEYS: "only-good", STATS_KV: new MemoryKV(), KV_STATS: "true" });
  try {
    await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), env);
    await worker.fetch(new Request("https://proxy.test/?apikey=client-good&s=Batman"), env);
    const response = await worker.fetch(new Request("https://proxy.test/metrics"), env);
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.requests.today, 2);
    assert.equal(json.requests.total, 2);
    assert.equal(json.requests.storage, "kv");
  } finally {
    await close(upstream);
  }
});

test("KV 统计读取时修正总数小于今日数的旧数据", async () => {
  const kv = new MemoryKV();
  const day = new Date().toISOString().slice(0, 10);
  await kv.put("requests:total", "32");
  await kv.put(`requests:day:${day}`, "42");

  const response = await worker.fetch(new Request("https://proxy.test/metrics"), testEnv("https://upstream.test/", { STATS_KV: kv, KV_STATS: "true" }));
  assert.equal(response.status, 200);
  const json = await response.json();
  assert.equal(json.requests.today, 42);
  assert.equal(json.requests.total, 42);
  assert.equal(await kv.get("requests:total"), "42");
});

test("KV 统计失败不影响正常代理请求", async () => {
  const upstream = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ Response: "True", Title: "Inception" }));
  });
  const base = await listen(upstream);
  const env = testEnv(base, { OMDB_KEYS: "only-good", STATS_KV: new FailingKV(), KV_STATS: "true" });
  try {
    const response = await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), env, {
      waitUntil(promise) {
        return promise.catch(() => {});
      }
    });
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.Response, "True");
  } finally {
    await close(upstream);
  }
});

test("D1 持久化统计总数、今日、成功率和失败率", async () => {
  let calls = 0;
  const upstream = http.createServer((req, res) => {
    calls += 1;
    res.setHeader("content-type", "application/json");
    if (calls === 1) {
      res.end(JSON.stringify({ Response: "True", Title: "Inception" }));
    } else {
      res.end(JSON.stringify({ Response: "False", Error: "Movie not found!" }));
    }
  });
  const base = await listen(upstream);
  const db = new MemoryD1();
  const waitUntilTasks = [];
  const ctx = { waitUntil(promise) { waitUntilTasks.push(promise); } };
  const env = testEnv(base, { OMDB_KEYS: "only-good", STATS_DB: db });
  try {
    await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), env, ctx);
    await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=NoSuchMovie"), env, ctx);
    await Promise.all(waitUntilTasks);
    const response = await worker.fetch(new Request("https://proxy.test/metrics"), env);
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.requests.storage, "d1");
    assert.equal(json.requests.total, 2);
    assert.equal(json.requests.today, 2);
    assert.equal(json.requests.success, 1);
    assert.equal(json.requests.failed, 1);
    assert.equal(json.requests.successRate, 50);
    assert.equal(json.requests.failureRate, 50);
    assert.ok(Array.isArray(json.requests.hourly));
    assert.ok(Array.isArray(json.requests.recent7Days));
  } finally {
    await close(upstream);
  }
});



test("从 KV 中读取 OMDb key 池", async () => {
  const upstream = http.createServer((req, res) => {
    const url = new URL(req.url, "http://upstream.test");
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ Response: "True", UsedKey: url.searchParams.get("apikey") }));
  });
  const base = await listen(upstream);
  const kv = new MemoryKV();
  await kv.put("omdb:keys", "kv-good");
  const env = testEnv(base, { OMDB_KEYS: "", STATS_KV: kv, CLIENT_KEYS: "client-good" });
  try {
    const response = await worker.fetch(new Request("https://proxy.test/?apikey=client-good&t=Inception"), env);
    assert.equal(response.status, 200);
    const json = await response.json();
    assert.equal(json.UsedKey, "kv-good");

    const health = await worker.fetch(new Request("https://proxy.test/health"), env);
    const healthJson = await health.json();
    assert.equal(healthJson.omdbKeySource, "kv");
  } finally {
    await close(upstream);
  }
});
