#pragma once

namespace ff {
// Chromium normally lays out a plain-text document as one preformatted block.
// Once a large document arrives, defer layout of offscreen sections. Keep the
// browser's decoded text, original URL, selection and scrolling. For very long
// individual lines, Blink's find-in-page cannot match across inline boxes.
inline constexpr char plain_text_layout[] = R"JS((() => {
  if (window !== window.top || document.contentType !== 'text/plain') return;
  const sectionSize = 16 * 1024;
  function partition(pre) {
    const text = pre.textContent;
    const sections = document.createDocumentFragment();
    const append = (content, lines, continued) => {
      const section = document.createElement(continued ? 'span' : 'div');
      if (continued) {
        // Inline-level boxes preserve copied text across soft wraps.
        section.style.display = 'inline-block';
        section.style.width = '100%';
        section.style.verticalAlign = 'top';
      }
      section.style.contentVisibility = 'auto';
      section.style.containIntrinsicBlockSize = 'auto ' + lines + 'lh';
      section.textContent = content;
      sections.append(section);
    };
    let start = 0;
    let end = 0;
    let lines = 0;
    while (end < text.length) {
      const newline = text.indexOf('\n', end);
      end = newline < 0 ? text.length : newline + 1;
      ++lines;
      if (lines < 256 && end - start < sectionSize && end < text.length) continue;
      if (end - start <= sectionSize * 4) {
        append(text.slice(start, end), lines, false);
      } else {
        // A single very long line would otherwise demand eager layout
        // of the entire file. Keep UTF-16 pairs intact at soft wraps.
        while (start < end) {
          let stop = Math.min(start + sectionSize, end);
          if (stop < end && text.charCodeAt(stop) >= 0xdc00 && text.charCodeAt(stop) <= 0xdfff) ++stop;
          append(text.slice(start, stop), 1, true);
          start = stop;
        }
      }
      start = end;
      lines = 0;
    }
    pre.replaceChildren(sections);
    pre.hidden = false;
  }

  const observer = new MutationObserver(() => {
    const pre = document.body?.querySelector('pre');
    if (!pre) return;
    let length = 0;
    for (const node of pre.childNodes) {
      length += node.length || 0;
      if (length < sectionSize * 4) continue;
      observer.disconnect();
      // The parser can still append text. Suppress full-document layout until
      // it finishes, then partition the decoded data at line boundaries.
      pre.hidden = true;
      if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', () => partition(pre), {once: true});
      } else {
        partition(pre);
      }
      break;
    }
  });
  observer.observe(document, {subtree:true, childList:true, characterData:true});
})())JS";
}
