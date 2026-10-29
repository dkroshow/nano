import { marked } from 'marked';
import { parseFileReference } from './file_references.js'
import { threadReferenceFromHref, threadReferenceHref } from './thread_references.js'

export function esc(s) {
  return String(s || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const _tblStartRe = /^\s*\u250c[\u2500\u252c]+\u2510/m;
const _tblEndRe = /^\s*\u2514[\u2500\u2534]+\u2518/m;
const _tblSepRe = /^[\u250c\u251c\u2514][\u2500\u252c\u253c\u2534\u2510\u2524\u2518]+$/;

function parseFence(line) {
  const m = String(line || '').match(/^( {0,3})(`{3,}|~{3,})(.*)$/);
  if (!m) return null;
  const marker = m[2];
  const info = m[3] || '';
  if (marker[0] === '`' && info.includes('`')) return null;
  return { char: marker[0], len: marker.length, info };
}

function isFenceClose(parsed, open) {
  return !!(parsed
    && parsed.char === open.char
    && parsed.len >= open.len
    && /^\s*$/.test(parsed.info));
}

export function boxTableToHtml(tableLines) {
  const sections = [];
  let cur = [];
  for (const line of tableLines) {
    if (_tblSepRe.test(line.trim())) {
      if (cur.length) {
        sections.push(cur);
        cur = [];
      }
    } else {
      cur.push(line);
    }
  }
  if (cur.length) sections.push(cur);
  if (!sections.length) return esc(tableLines.join('\n'));

  let html = '<div class="box-table-wrap"><table class="box-table">';
  sections.forEach((rows, sIdx) => {
    const isHdr = sIdx === 0;
    const tag = isHdr ? 'th' : 'td';
    const cells = [];
    for (const line of rows) {
      const parts = line.split('\u2502');
      if (parts.length < 3) continue;
      const ct = parts.slice(1, -1).map(p => p.trim());
      if (!cells.length) {
        ct.forEach(t => cells.push([t]));
      } else {
        ct.forEach((t, i) => {
          if (i < cells.length) cells[i].push(t);
          else cells.push([t]);
        });
      }
    }
    if (isHdr) html += '<thead>';
    html += '<tr>';
    for (const cell of cells) {
      html += '<' + tag + '>' + cell.filter(Boolean).map(t => esc(t)).join('<br>') + '</' + tag + '>';
    }
    html += '</tr>';
    if (isHdr) html += '</thead>';
  });
  html += '</table></div>';
  return html;
}

const renderer = new marked.Renderer();
renderer.link = function ({ href, title, tokens }) {
  return linkHtml(href, title, this.parser.parseInline(tokens));
};
// An image becomes a link, never an <img>: an <img> would make the operator's
// browser fetch whatever URL an agent wrote, and that is the page's only
// network egress. One rule for every URL form, the same as a link's.
renderer.image = function ({ href, title, text }) {
  const target = String(href ?? '');
  return linkHtml(target, title, esc(text || target));
};
function linkHtml(href, title, text) {
  const target = String(href ?? '');
  const attr = (v) => esc(v).replace(/`/g, '&#96;');
  const titleAttr = title ? ' title="' + attr(title) + '"' : '';
  const threadReference = threadReferenceFromHref(target)
  if (threadReference) {
    return '<a href="' + attr(threadReferenceHref(threadReference)) + '" class="nano-thread-link" data-nano-thread-reference="' + attr(threadReference) + '"' +
      titleAttr + '>' + text + '</a>';
  }
  const file = parseFileReference(target, { explicit: true });
  if (file) {
    return '<a href="#" class="nano-file-link" data-nano-file-path="' + attr(file.path) + '"' +
      (file.line ? ' data-file-line="' + file.line + '"' : '') +
      titleAttr + '>' + text + '</a>';
  }
  if (!/^(https?:|mailto:|#)/i.test(target)) return text;
  return '<a href="' + attr(target) + '" target="_blank" rel="noopener noreferrer"' + titleAttr + '>' + text + '</a>';
}
const tokenizer = new marked.Tokenizer();
tokenizer.html = () => undefined;
tokenizer.tag = () => undefined;
// Per-call options, not the shared marked defaults, which the streaming
// surface lexes with.
const markedOptions = { renderer, tokenizer };

export function md(s) {
  try {
    const allLines = String(s || '').split('\n');
    const segments = [];
    let textBuf = [];
    let li = 0;
    let scanFence = null;
    while (li < allLines.length) {
      const line = allLines[li];
      const fence = parseFence(line);
      if (scanFence) {
        textBuf.push(line);
        if (isFenceClose(fence, scanFence)) scanFence = null;
        li++;
        continue;
      }
      if (fence) {
        scanFence = fence;
        textBuf.push(line);
        li++;
        continue;
      }
      if (_tblStartRe.test(line)) {
        if (textBuf.length) {
          segments.push({ t: 'text', l: textBuf });
          textBuf = [];
        }
        const tl = [];
        let j = li;
        let found = false;
        while (j < allLines.length) {
          tl.push(allLines[j]);
          if (_tblEndRe.test(allLines[j]) && j > li) {
            found = true;
            j++;
            break;
          }
          j++;
        }
        if (found) segments.push({ t: 'table', l: tl });
        else textBuf.push(...tl);
        li = j;
      } else {
        textBuf.push(line);
        li++;
      }
    }
    if (textBuf.length) segments.push({ t: 'text', l: textBuf });

    const boxRe = /[\u2500-\u257f\u2580-\u259f]/;
    let html = '';
    for (const seg of segments) {
      if (seg.t === 'table') {
        html += boxTableToHtml(seg.l);
        continue;
      }
      const out = [];
      let inBox = false;
      let boxFence = null;
      for (const line of seg.l) {
        const fence = parseFence(line);
        if (boxFence) {
          if (inBox) {
            out.push('```');
            inBox = false;
          }
          out.push(line);
          if (isFenceClose(fence, boxFence)) boxFence = null;
          continue;
        }
        if (fence) {
          if (inBox) {
            out.push('```');
            inBox = false;
          }
          boxFence = fence;
          out.push(line);
          continue;
        }
        if (boxRe.test(line)) {
          if (!inBox) {
            out.push('```');
            inBox = true;
          }
          out.push(line);
        } else {
          if (inBox) {
            out.push('```');
            inBox = false;
          }
          out.push(line);
        }
      }
      if (inBox) out.push('```');
      html += marked.parse(out.join('\n'), markedOptions);
    }
    return html.replace(/<table>/g, '<div class="md-table-wrap"><table>')
      .replace(/<\/table>/g, '</table></div>');
  } catch (_) {
  }
  return '<div class="md-plain">' + esc(s) + '</div>';
}
