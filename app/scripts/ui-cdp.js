// ui-cdp.js — драйвер UI-тестов через Chrome DevTools Protocol (WebView2).
// Запуск приложения: ENV=local VOX_REMOTE_DEBUG_PORT=9222 ./vox.exe
// (порт открывается только в ENV=local, см. remoteDebugArgs в main.go).
// Затем из любой директории: node app/scripts/ui-cdp.js <cmd>
//   shot out.png          — скриншот страницы (PNG)
//   eval "<js>"           — выполнить JS в странице, вывести результат
//   console <sec>         — собрать console/exception события за N секунд
//   net <sec> [reload]    — неуспешные HTTP-ответы/сетевые ошибки (опц. с reload)
//   pages                 — список CDP-целей
// Каждый eval выполняется в общем глобальном scope страницы — оборачивайте в IIFE.

const fs = require("fs");
const PORT = process.env.CDP_PORT || 9222;

async function targets() {
    const r = await fetch(`http://127.0.0.1:${PORT}/json`);
    return r.json();
}

async function connect(filter) {
    const list = await targets();
    // CDP_FILTER="=<url>" — точное совпадение URL (главное окно "http://wails.localhost/",
    // когда открыты ещё screen-view.html / notify.html — includes их тоже ловит).
    const match = (u) => !filter ? true : filter.startsWith("=") ? u === filter.slice(1) : u.includes(filter);
    let t = list.find(x => x.type === "page" && match(x.url || ""));
    if (!t) t = list.find(x => x.type === "page");
    if (!t) throw new Error("no page target; targets=" + JSON.stringify(list));
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0;
    const pending = new Map();
    const events = [];
    ws.onmessage = (m) => {
        const d = JSON.parse(m.data);
        if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); }
        else if (d.method) events.push(d);
    };
    const send = (method, params = {}) => new Promise((res) => {
        const i = ++id;
        pending.set(i, res);
        ws.send(JSON.stringify({ id: i, method, params }));
    });
    return { ws, send, events, target: t };
}

(async () => {
    const [cmd, a1, a2] = process.argv.slice(2);
    if (cmd === "pages") { console.log(JSON.stringify(await targets(), null, 1)); return; }
    const c = await connect(process.env.CDP_FILTER);
    if (cmd === "shot") {
        await c.send("Page.enable");
        const r = await c.send("Page.captureScreenshot", { format: "png" });
        fs.writeFileSync(a1, Buffer.from(r.result.data, "base64"));
        console.log("saved", a1, "url=", c.target.url);
    } else if (cmd === "eval") {
        await c.send("Runtime.enable");
        // eval @path.js — выражение из файла (аргумент командной строки на
        // Windows ограничен ~32K символов; большие payload'ы, например
        // base64-кадр для проверки WebCodecs, так не передать).
        const expression = a1 && a1.startsWith("@") ? fs.readFileSync(a1.slice(1), "utf8") : a1;
        const r = await c.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
        if (r.result.exceptionDetails) console.log("EXCEPTION:", JSON.stringify(r.result.exceptionDetails, null, 1));
        else console.log(JSON.stringify(r.result.result.value, null, 1));
    } else if (cmd === "net") {
        // net <seconds> [reload] — логируем неуспешные ответы/ошибки сети
        await c.send("Network.enable");
        if (a2 === "reload") { await c.send("Page.enable"); await c.send("Page.reload"); }
        await new Promise(r => setTimeout(r, (parseInt(a1 || "5", 10)) * 1000));
        for (const e of c.events) {
            if (e.method === "Network.responseReceived" && e.params.response.status >= 400)
                console.log("[HTTP " + e.params.response.status + "]", e.params.response.url, e.params.type);
            else if (e.method === "Network.loadingFailed")
                console.log("[FAILED]", e.params.requestId, e.params.errorText, e.params.type);
        }
    } else if (cmd === "console") {
        await c.send("Runtime.enable");
        await c.send("Log.enable");
        await new Promise(r => setTimeout(r, (parseInt(a1 || "3", 10)) * 1000));
        for (const e of c.events) {
            if (e.method === "Runtime.consoleAPICalled") {
                const args = e.params.args.map(x => x.value !== undefined ? x.value : (x.description || x.type)).join(" ");
                console.log("[" + e.params.type + "]", args.slice(0, 300));
            } else if (e.method === "Runtime.exceptionThrown") {
                console.log("[EXC]", JSON.stringify(e.params.exceptionDetails).slice(0, 400));
            } else if (e.method === "Log.entryAdded") {
                console.log("[log:" + e.params.entry.level + "]", (e.params.entry.text || "").slice(0, 300));
            }
        }
    }
    c.ws.close();
})().catch(e => { console.error("CDP error:", e.message); process.exit(1); });
