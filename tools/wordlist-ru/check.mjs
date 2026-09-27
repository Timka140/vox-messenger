// Сводит черновые списки, проверяет правила и отбирает слова для русского словаря фраз.
// node check.mjs [target=2048]
import { readFileSync, writeFileSync, existsSync } from 'node:fs';

const dir = new URL('./', import.meta.url);
const target = Number(process.argv[2] || 2048);
const sources = ['nature', 'home', 'city', 'misc', 'extra'];

const reasons = {};
const reject = (w, why) => { (reasons[why] ||= []).push(w); };

const seen = new Set();
const pool = [];
for (const s of sources) {
  const f = new URL(`${s}.txt`, dir);
  if (!existsSync(f)) continue;
  for (let w of readFileSync(f, 'utf8').split(/\r?\n/)) {
    w = w.trim().toLowerCase();
    if (!w) continue;
    if (w.includes('ё')) { reject(w, 'ё'); continue; }
    if (!/^[а-я]+$/.test(w)) { reject(w, 'не только а–я'); continue; }
    if (w.length < 4 || w.length > 8) { reject(w, 'длина'); continue; }
    if (seen.has(w)) { reject(w, 'повтор'); continue; }
    seen.add(w);
    pool.push({ w, src: s });
  }
}

// Расстояние Левенштейна ровно 1 (замена, вставка или удаление одной буквы).
const dist1 = (a, b) => {
  if (Math.abs(a.length - b.length) > 1 || a === b) return false;
  if (a.length === b.length) {
    let d = 0;
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i] && ++d > 1) return false;
    return d === 1;
  }
  const [s, l] = a.length < b.length ? [a, b] : [b, a];
  let i = 0, j = 0, skipped = false;
  while (i < s.length && j < l.length) {
    if (s[i] === l[j]) { i++; j++; continue; }
    if (skipped) return false;
    skipped = true; j++;
  }
  return true;
};
// Перестановка соседних букв тоже путается при вводе.
const swap1 = (a, b) => {
  if (a.length !== b.length) return false;
  const d = [];
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) d.push(i);
  return d.length === 2 && d[1] === d[0] + 1 && a[d[0]] === b[d[1]] && a[d[1]] === b[d[0]];
};

// Жадный отбор: короче и «проще» сначала не сортируем — порядок источников, чтобы темы перемешались равномерно.
const byPrefix = new Map();
const chosen = [];
const order = [];
const buckets = sources.map((s) => pool.filter((p) => p.src === s));
for (let k = 0; order.length < pool.length; k++) {
  for (const b of buckets) if (k < b.length) order.push(b[k]);
}
for (const { w } of order) {
  const p = w.slice(0, 4);
  if (byPrefix.has(p)) { reject(w, `начало «${p}» как у «${byPrefix.get(p)}»`.slice(0, 0) + 'одинаковые первые 4 буквы'); continue; }
  const clash = chosen.find((c) => dist1(c, w) || swap1(c, w));
  if (clash) { reject(w, 'отличается одной буквой'); continue; }
  byPrefix.set(p, w);
  chosen.push(w);
}

const final = chosen.slice(0, target).sort((a, b) => a.localeCompare(b, 'ru'));
writeFileSync(new URL('ru-candidates.txt', dir), final.join('\n') + '\n');
console.log(`в черновиках: ${pool.length + Object.values(reasons).flat().length}, прошли проверки: ${chosen.length}, записано: ${final.length}, не хватает: ${Math.max(0, target - chosen.length)}`);
for (const [why, ws] of Object.entries(reasons)) console.log(`  отброшено (${why}): ${ws.length}`);
