export function isMarkdownStructuralLine(line) {
  const trimmed = String(line || '').trimStart();
  if (!trimmed) return false;
  if (/^(#{1,6})\s/.test(trimmed)) return true;
  if (/^(?:[-*_]\s*){3,}$/.test(trimmed)) return true;
  if (/^>/.test(trimmed)) return true;
  if (/^\|.*\|\s*$/.test(trimmed)) return true;
  if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) return true;
  if (/^ {2,}\S/.test(line) || /^\t/.test(line)) return true;
  if (/^ {0,3}(```|~~~)/.test(line)) return true;
  if (/^\[[^\]]+\]:\s+\S/.test(trimmed)) return true;
  if (/^<\/?[A-Za-z][^>]*>\s*$/.test(trimmed)) return true;
  return false;
}

export function unwrapMarkdownProse(text) {
  const lines = String(text || '').replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  let para = [];
  let inFence = false;
  let inFrontmatter = false;

  function flushPara() {
    if (!para.length) return;
    out.push(para.map((line) => line.trim()).join(' ').replace(/\s+/g, ' ').trim());
    para = [];
  }

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    const trimmed = line.trim();

    if (i === 0 && /^---\s*$/.test(line)) {
      flushPara();
      out.push(line);
      inFrontmatter = true;
      continue;
    }
    if (inFrontmatter) {
      out.push(line);
      if (/^---\s*$/.test(line)) inFrontmatter = false;
      continue;
    }

    if (/^ {0,3}(```|~~~)/.test(line)) {
      flushPara();
      out.push(line);
      inFence = !inFence;
      continue;
    }
    if (inFence) {
      out.push(line);
      continue;
    }
    if (trimmed === '') {
      flushPara();
      out.push(line);
      continue;
    }
    if (isMarkdownStructuralLine(line)) {
      flushPara();
      out.push(line);
      continue;
    }
    para.push(line);
  }

  flushPara();
  return out.join('\n');
}
