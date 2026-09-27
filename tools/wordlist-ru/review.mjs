// Собирает страницу вычитки русского словаря фраз: node review.mjs → ru-wordlist-review.html
import { readFileSync, writeFileSync } from 'node:fs';

const dir = new URL('./', import.meta.url);
const words = readFileSync(new URL('ru-candidates.txt', dir), 'utf8').split(/\r?\n/).filter(Boolean);
const SUGGEST = {
  'путается «е» и «ё»': ['черта', 'тетушка'],
  'двусмысленное': ['пикап', 'санузел', 'извилина'],
  'трудное написание': ['брокколи', 'спагетти', 'мокасин', 'шиншилла', 'иероглиф'],
  'редкое слово': ['сулугуни', 'ламантин', 'афалина', 'фенек', 'ирбис', 'криница', 'чинара', 'гопак', 'водевиль', 'оранжад', 'тахометр', 'суглинок', 'гумус', 'делянка', 'угодье', 'межа', 'тутовник', 'казначей', 'этнограф'],
  'уменьшительное рядом с похожим': ['шашечка', 'часики', 'розочка', 'лодочка', 'сумочка', 'рощица', 'куколка', 'лужок', 'ломтик', 'избушка', 'флажок', 'окошко', 'туфелька', 'ситечко', 'лесенка', 'зубок', 'зубчик', 'петелька', 'мышка', 'пампушка'],
};
const suggest = {};
for (const [why, ws] of Object.entries(SUGGEST)) for (const w of ws) {
  if (!words.includes(w)) throw new Error('нет в списке: ' + w);
  suggest[w] = why;
}

const html = `<title>Словарь фраз Vox</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Open+Sans:wght@400;600;700&display=swap">
<style>
  :root {
    --bg: #f0f2f5; --panel: #ffffff; --line: #e1e4e8; --text: #1f2328; --muted: #656d76; --faint: #8c959f;
    --accent: #3f9a86; --accent-soft: rgba(63,154,134,0.12); --warn: #b7791f; --warn-soft: rgba(183,121,31,0.10); --bad: #cf222e; --bad-soft: rgba(207,34,46,0.08);
  }
  @media (prefers-color-scheme: dark) {
    :root:not([data-theme="light"]) {
      --bg: #242830; --panel: #2E323B; --line: #3A3E47; --text: #e8eaed; --muted: #8b929e; --faint: #636a76;
      --accent: #A3DAC5; --accent-soft: rgba(163,218,197,0.14); --warn: #faa61a; --warn-soft: rgba(250,166,26,0.10); --bad: #f47474; --bad-soft: rgba(240,71,71,0.12);
    }
  }
  :root[data-theme="dark"] {
    --bg: #242830; --panel: #2E323B; --line: #3A3E47; --text: #e8eaed; --muted: #8b929e; --faint: #636a76;
    --accent: #A3DAC5; --accent-soft: rgba(163,218,197,0.14); --warn: #faa61a; --warn-soft: rgba(250,166,26,0.10); --bad: #f47474; --bad-soft: rgba(240,71,71,0.12);
  }
  * { box-sizing: border-box; }
  body { background: var(--bg); color: var(--text); font-family: 'Open Sans', 'Segoe UI', system-ui, sans-serif; font-size: 14px; }
  .wrap { max-width: 1120px; margin: 0 auto; padding: 24px 16px 64px; display: flex; flex-direction: column; gap: 20px; }
  h1 { font-size: 22px; margin: 0; text-wrap: balance; }
  p { margin: 0; line-height: 1.55; color: var(--muted); max-width: 70ch; }
  .bar { position: sticky; top: env(safe-area-inset-top, 0px); z-index: 2; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 12px 14px; display: flex; flex-wrap: wrap; gap: 12px 20px; align-items: center; }
  .stat { display: flex; flex-direction: column; gap: 1px; }
  .stat b { font-size: 18px; font-variant-numeric: tabular-nums; }
  .stat span { font-size: 12px; color: var(--muted); }
  .stat.ok b { color: var(--accent); } .stat.low b { color: var(--bad); }
  .tools { margin-left: auto; display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
  input[type=search] { height: 34px; border-radius: 6px; border: 1px solid var(--line); background: var(--bg); color: var(--text); padding: 0 10px; font: inherit; width: 180px; }
  .seg { display: flex; border: 1px solid var(--line); border-radius: 6px; padding: 2px; gap: 2px; background: var(--bg); }
  .seg button { border: 0; background: transparent; color: var(--muted); font: inherit; font-size: 13px; font-weight: 600; padding: 5px 10px; border-radius: 4px; cursor: pointer; }
  .seg button[aria-pressed="true"] { background: var(--panel); color: var(--text); box-shadow: 0 0 0 1px var(--line); }
  .btn { height: 34px; border-radius: 6px; border: 1px solid var(--line); background: transparent; color: var(--text); font: inherit; font-size: 13px; font-weight: 600; padding: 0 12px; cursor: pointer; }
  .btn:hover { background: var(--bg); }
  button:focus-visible, input:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .legend { display: flex; flex-wrap: wrap; gap: 8px 18px; font-size: 13px; color: var(--muted); }
  .legend span { display: inline-flex; align-items: center; gap: 6px; }
  .letter { display: grid; grid-template-columns: 36px minmax(0, 1fr); gap: 10px; align-items: start; }
  .letter > h2 { margin: 0; font-size: 18px; color: var(--faint); line-height: 30px; text-align: center; }
  .chips { display: flex; flex-wrap: wrap; gap: 6px; }
  .w { font: inherit; font-size: 14px; height: 30px; padding: 0 10px; border-radius: 6px; border: 1px solid var(--line); background: var(--panel); color: var(--text); cursor: pointer; }
  .w:hover { border-color: var(--muted); }
  .w.sug { border: 1px dashed var(--warn); background: var(--warn-soft); }
  .w.off { background: var(--bad-soft); border: 1px solid transparent; color: var(--bad); text-decoration: line-through; }
  .sample { font-size: 12px; height: 22px; padding: 0 7px; border-radius: 4px; display: inline-flex; align-items: center; border: 1px solid var(--line); background: var(--panel); color: var(--text); }
  .out { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; display: flex; flex-direction: column; gap: 10px; }
  textarea { width: 100%; min-height: 90px; resize: vertical; border-radius: 6px; border: 1px solid var(--line); background: var(--bg); color: var(--text); font: 13px/1.5 Consolas, 'SF Mono', monospace; padding: 8px 10px; }
  .why { font-size: 12px; color: var(--muted); }
  #empty { color: var(--muted); }
</style>

<div class="wrap">
  <header style="display: flex; flex-direction: column; gap: 8px;">
    <h1>Словарь секретных фраз Vox</h1>
    <p>Из этого списка Vox берёт 12 слов для фразы. Нужно ровно 2048 слов, кандидатов ${words.length}. Щёлкните слово, чтобы убрать его или вернуть. Жёлтой рамкой отмечены слова, которые я предлагаю убрать, — наведите, чтобы увидеть причину. Внизу — список убранных для отправки.</p>
  </header>

  <div class="bar">
    <div class="stat" id="st-left"><b id="left">0</b><span>останется</span></div>
    <div class="stat"><b id="removed">0</b><span>убрано</span></div>
    <div class="stat" id="st-spare"><b id="spare">0</b><span>запас сверх 2048</span></div>
    <div class="tools">
      <input type="search" id="q" placeholder="Найти слово" aria-label="Найти слово">
      <div class="seg" role="group" aria-label="Показать">
        <button type="button" data-f="all" aria-pressed="true">Все</button>
        <button type="button" data-f="sug" aria-pressed="false">Предложенные</button>
        <button type="button" data-f="off" aria-pressed="false">Убранные</button>
      </div>
      <button type="button" class="btn" id="take-sug">Убрать все предложенные</button>
    </div>
  </div>

  <div class="legend">
    <span><i class="sample">слово</i> в списке</span>
    <span><i class="sample" style="border: 1px dashed var(--warn); background: var(--warn-soft);">слово</i> предлагаю убрать</span>
    <span><i class="sample" style="color: var(--bad); background: var(--bad-soft); border-color: transparent; text-decoration: line-through;">слово</i> убрано</span>
  </div>

  <main id="list" style="display: flex; flex-direction: column; gap: 14px;"></main>
  <p id="empty" hidden>Ничего не найдено.</p>

  <section class="out">
    <div style="display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px;">
      <b>Убранные слова — скопируйте и отправьте в чат</b>
      <button type="button" class="btn" id="copy">Скопировать</button>
    </div>
    <textarea id="removed-list" readonly aria-label="Убранные слова"></textarea>
    <span class="why">Убранные запоминаются в этом браузере — страницу можно закрыть и вернуться.</span>
  </section>
</div>

<script>
const WORDS = ${JSON.stringify(words)};
const SUGGEST = ${JSON.stringify(suggest)};
const TARGET = 2048;
const KEY = 'vox-ru-wordlist-removed';
let removed = new Set();
try { removed = new Set(JSON.parse(localStorage.getItem(KEY) || '[]')); } catch (e) {}
let filter = 'all';
const save = () => { try { localStorage.setItem(KEY, JSON.stringify([...removed])); } catch (e) {} };

const list = document.getElementById('list');
const groups = new Map();
for (const w of WORDS) {
  const l = w[0];
  if (!groups.has(l)) groups.set(l, []);
  groups.get(l).push(w);
}
const buttons = new Map();
for (const [l, ws] of groups) {
  const sec = document.createElement('section');
  sec.className = 'letter';
  sec.innerHTML = '<h2>' + l.toUpperCase() + '</h2>';
  const chips = document.createElement('div');
  chips.className = 'chips';
  for (const w of ws) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'w' + (SUGGEST[w] ? ' sug' : '');
    b.textContent = w;
    if (SUGGEST[w]) b.title = 'Предлагаю убрать: ' + SUGGEST[w];
    b.addEventListener('click', () => { removed.has(w) ? removed.delete(w) : removed.add(w); save(); render(); });
    chips.appendChild(b);
    buttons.set(w, b);
  }
  sec.appendChild(chips);
  list.appendChild(sec);
}

function render() {
  const q = document.getElementById('q').value.trim().toLowerCase();
  let shown = 0;
  for (const [w, b] of buttons) {
    const off = removed.has(w);
    b.classList.toggle('off', off);
    b.setAttribute('aria-pressed', off ? 'true' : 'false');
    const pass = (!q || w.includes(q)) && (filter === 'all' || (filter === 'sug' && SUGGEST[w]) || (filter === 'off' && off));
    b.hidden = !pass;
    if (pass) shown++;
  }
  for (const sec of list.children) sec.hidden = ![...sec.querySelectorAll('.w')].some((b) => !b.hidden);
  document.getElementById('empty').hidden = shown > 0;
  const left = WORDS.length - removed.size;
  document.getElementById('left').textContent = left;
  document.getElementById('removed').textContent = removed.size;
  document.getElementById('spare').textContent = left - TARGET;
  document.getElementById('st-spare').className = 'stat ' + (left >= TARGET ? 'ok' : 'low');
  document.getElementById('removed-list').value = [...removed].sort((a, b) => a.localeCompare(b, 'ru')).join(' ');
}
document.getElementById('q').addEventListener('input', render);
for (const b of document.querySelectorAll('.seg button')) b.addEventListener('click', () => {
  filter = b.dataset.f;
  for (const x of document.querySelectorAll('.seg button')) x.setAttribute('aria-pressed', x === b ? 'true' : 'false');
  render();
});
document.getElementById('take-sug').addEventListener('click', () => { for (const w of Object.keys(SUGGEST)) removed.add(w); save(); render(); });
document.getElementById('copy').addEventListener('click', async (e) => {
  const ta = document.getElementById('removed-list');
  try { await navigator.clipboard.writeText(ta.value); e.target.textContent = 'Скопировано'; }
  catch (err) { ta.select(); e.target.textContent = 'Выделено — нажмите Ctrl+C'; }
  setTimeout(() => { e.target.textContent = 'Скопировать'; }, 2000);
});
render();
</script>
`;
writeFileSync(new URL('ru-wordlist-review.html', dir), html);
console.log('ok', words.length, Object.keys(suggest).length);
