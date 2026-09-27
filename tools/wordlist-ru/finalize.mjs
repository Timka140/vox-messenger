// Итоговый русский словарь: кандидаты минус предложенные к удалению, лишнее — сначала самые длинные.
import { readFileSync, writeFileSync } from 'node:fs';
const src = readFileSync(new URL('review.mjs', import.meta.url), 'utf8');
const sug = [...src.slice(src.indexOf('const SUGGEST'), src.indexOf('};', src.indexOf('const SUGGEST'))).matchAll(/'([а-я]+)'/g)].map((m) => m[1]);
const all = readFileSync(new URL('ru-candidates.txt', import.meta.url), 'utf8').split(/\r?\n/).filter(Boolean);
let keep = all.filter((w) => !sug.includes(w));
const extra = keep.length - 2048;
const drop = new Set([...keep].sort((a, b) => b.length - a.length || b.localeCompare(a, 'ru')).slice(0, extra));
keep = keep.filter((w) => !drop.has(w)).sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
if (keep.length !== 2048) throw new Error('count ' + keep.length);
writeFileSync(new URL('../../app/internal/identity/wordlist_ru.txt', import.meta.url), keep.join('\n') + '\n');
console.log('suggested removed', sug.length, 'dropped by length', drop.size, 'final', keep.length);
