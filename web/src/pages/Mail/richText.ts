/**
 * Helpers shared by the rich-text composer, signatures, and
 * templates.
 *
 * These convert between the HTML the TipTap editor produces and the
 * plain text / cid-referenced HTML the JMAP send path needs, and
 * provide a conservative HTML→text fallback so a recipient on a
 * text-only client still gets a readable message.
 */

/**
 * Convert an HTML fragment to a readable plain-text approximation.
 * Block-level tags become newlines, `<br>` becomes a single
 * newline, list items get a leading bullet, then all remaining tags
 * are stripped and entities decoded. Runs purely on string
 * manipulation so it works in both the browser and jsdom (no
 * DOMParser dependency, which jsdom supports but Node workers may
 * not).
 */
export function htmlToPlainText(html: string): string {
  if (!html) return "";
  let text = html;
  // Drop script/style wholesale — their contents are never body text.
  text = text.replace(/<style[\s\S]*?<\/style>/gi, "");
  text = text.replace(/<script[\s\S]*?<\/script>/gi, "");
  // List items → "- item".
  text = text.replace(/<li[^>]*>/gi, "\n- ");
  // Common block boundaries → newline.
  text = text.replace(/<\/(p|div|h[1-6]|ul|ol|li|blockquote|tr)>/gi, "\n");
  text = text.replace(/<br\s*\/?>/gi, "\n");
  // Strip every remaining tag.
  text = text.replace(/<[^>]+>/g, "");
  // Decode the handful of entities the editor emits.
  text = text
    .replace(/&nbsp;/g, " ")
    .replace(/&amp;/g, "&")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'");
  // Collapse the runs of blank lines the block replacements create.
  text = text.replace(/\n{3,}/g, "\n\n");
  return text.trim();
}

/**
 * True when an HTML string carries no visible content (only empty
 * tags / whitespace), e.g. TipTap's `<p></p>` for an empty editor.
 * Used so an untouched rich-text body is treated as "no body".
 */
export function isHtmlEmpty(html: string): boolean {
  return htmlToPlainText(html).length === 0;
}

/**
 * Escape text for safe interpolation into an HTML context. Used to
 * turn a plain-text body / signature into HTML without letting
 * `<`, `&`, etc. inject markup.
 */
export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/**
 * Wrap a plain-text string as an HTML fragment, preserving line
 * breaks. Used when toggling a plain-text body into rich-text mode.
 */
export function plainTextToHtml(text: string): string {
  if (!text) return "";
  return text
    .split(/\r?\n/)
    .map((line) => (line.length === 0 ? "<p></p>" : `<p>${escapeHtml(line)}</p>`))
    .join("");
}
